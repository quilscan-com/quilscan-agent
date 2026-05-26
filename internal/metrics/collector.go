// Package metrics samples host + node-process resource usage and pushes a
// `metrics` frame to backend on a fixed cadence. The frame shape mirrors what
// the my-nodes page reads from agentSnapshot.metrics — see frontend
// composables/useAgent.js for the consumer side.
package metrics

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
)

// Sender abstracts the WS client for testability.
type Sender interface {
	Send(v interface{}) error
}

// ServiceProbe is the narrow contract the collector needs to surface a
// `node_running` bool — IsActive returns true when the named service unit
// is currently running. Implemented by svcctl.Ctl on both platforms.
type ServiceProbe interface {
	IsActive(name string) bool
}

// Collector is the rolling sampler. Run() emits one `metrics` frame per Tick
// while a browser is actively streaming, and per IdleTick otherwise.
type Collector struct {
	Sender   Sender
	Tick     time.Duration
	IdleTick time.Duration
	Started  time.Time

	// DiskPath is the partition whose usage we report (default "/").
	DiskPath string

	// HostUptime returns the host OS uptime in seconds. nil uses the platform
	// collector from gopsutil.
	HostUptime func() (uint64, error)

	// NetDevPath is the Linux /proc/net/dev path used for aggregate network
	// counters. Empty uses the platform default collector.
	NetDevPath string

	// UnitName is the platform service unit of the quilibrium-node
	// (e.g. "quilibrium-node.service" on Linux, "com.quilscan.node" on
	// macOS). When non-empty and Svc is set, we probe IsActive each tick
	// to surface a `node_running` bool.
	UnitName string

	// Svc is the platform service controller. nil → node_running not emitted.
	Svc ServiceProbe

	// Internal sample state — mutex-guarded because we may eventually expose
	// stats over a side channel.
	mu          sync.Mutex
	cpuPoints   []float64
	memPoints   []float64
	diskPoints  []float64
	cpuStaticOK bool
	cpuModel    string
	cpuCores    int
	netLast     netCounters
	netLastAt   time.Time
	streaming   bool
	modeCh      chan struct{}
}

const (
	pointsCapacity = 24 // ~ 1 minute of data at 3s tick
	defaultNetDevPath = "/proc/net/dev"
)

var darwinTopIdlePattern = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)%\s+idle`)

// Run blocks until ctx is cancelled. Errors collecting individual samples are
// swallowed (with the affected field omitted) so a transient gopsutil failure
// doesn't crash the agent.
func (c *Collector) Run(ctx context.Context) {
	if c.Tick <= 0 {
		c.Tick = 3 * time.Second
	}
	if c.IdleTick <= 0 {
		c.IdleTick = c.Tick
	}
	if c.DiskPath == "" {
		c.DiskPath = "/"
	}
	c.warmCPUStatic()

	modeCh := c.modeSignal()
	timer := time.NewTimer(c.currentTick())
	defer timer.Stop()
	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(c.currentTick())
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			c.tick()
			timer.Reset(c.currentTick())
		case <-modeCh:
			if c.isStreaming() {
				c.tick()
			}
			resetTimer()
		}
	}
}

func (c *Collector) SetStreaming(on bool) {
	c.mu.Lock()
	if c.streaming == on {
		c.mu.Unlock()
		return
	}
	c.streaming = on
	ch := c.ensureModeChLocked()
	c.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (c *Collector) currentTick() time.Duration {
	c.mu.Lock()
	streaming := c.streaming
	tick := c.Tick
	idleTick := c.IdleTick
	c.mu.Unlock()
	if tick <= 0 {
		tick = 3 * time.Second
	}
	if idleTick <= 0 {
		idleTick = tick
	}
	if streaming {
		return tick
	}
	return idleTick
}

func (c *Collector) isStreaming() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streaming
}

func (c *Collector) modeSignal() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ensureModeChLocked()
}

func (c *Collector) ensureModeChLocked() chan struct{} {
	if c.modeCh == nil {
		c.modeCh = make(chan struct{}, 1)
	}
	return c.modeCh
}

func (c *Collector) tick() {
	frame := c.sample()
	if c.Sender != nil {
		_ = c.Sender.Send(frame)
	}
}

// sample collects one snapshot and returns the JSON-serializable frame.
// Exposed for testing.
func (c *Collector) sample() map[string]interface{} {
	frame := map[string]interface{}{
		"type": "metrics",
	}

	if v, ok := hostCPUPercent(); ok {
		frame["cpu"] = v
		c.appendPoint(&c.cpuPoints, v)
	}
	if c.cpuStaticOK {
		frame["cpu_sub"] = fmt.Sprintf("%d Core - %s", c.cpuCores, trimModel(c.cpuModel))
	}
	if pts := c.snapshotPoints(c.cpuPoints); pts != nil {
		frame["cpu_points"] = pts
	}

	if vm, err := mem.VirtualMemory(); err == nil {
		v := round1(vm.UsedPercent)
		frame["mem"] = v
		frame["mem_sub"] = fmt.Sprintf("%s / %s", humanBytes(vm.Used), humanBytes(vm.Total))
		c.appendPoint(&c.memPoints, v)
	}
	if pts := c.snapshotPoints(c.memPoints); pts != nil {
		frame["mem_points"] = pts
	}

	if du, err := disk.Usage(c.DiskPath); err == nil {
		v := round1(du.UsedPercent)
		frame["disk"] = v
		frame["disk_sub"] = fmt.Sprintf("%s / %s", humanBytes(du.Used), humanBytes(du.Total))
		c.appendPoint(&c.diskPoints, v)
	}
	if pts := c.snapshotPoints(c.diskPoints); pts != nil {
		frame["disk_points"] = pts
	}

	if uptime, ok := c.systemUptime(); ok {
		frame["system_uptime_sec"] = int64(uptime)
		frame["system_uptime_sub"] = shortUptime(uptime)
	}

	if nc, ok := readNetCounters(c.NetDevPath); ok {
		frame["net_rx_bytes"] = nc.RXBytes
		frame["net_tx_bytes"] = nc.TXBytes
		frame["net_total_sub"] = fmt.Sprintf("RX %s / TX %s", humanBytes(nc.RXBytes), humanBytes(nc.TXBytes))
		if down, up, ok := c.netRates(nc, time.Now()); ok {
			frame["net_down_mbit_s"] = down
			frame["net_up_mbit_s"] = up
			frame["net_speed_sub"] = fmt.Sprintf("Down %.2f Mbit/s / Up %.2f Mbit/s", down, up)
		}
	}

	if rss := nodeProcessRSSBytes(); rss > 0 {
		frame["node_mem_bytes"] = rss
		frame["node_mem_sub"] = humanBytes(uint64(rss))
	}

	if c.UnitName != "" && c.Svc != nil {
		frame["node_running"] = c.Svc.IsActive(c.UnitName)
	}

	if !c.Started.IsZero() {
		frame["uptime_sec"] = int64(time.Since(c.Started).Seconds())
	}

	return frame
}

func (c *Collector) systemUptime() (uint64, bool) {
	fn := c.HostUptime
	if fn == nil {
		fn = host.Uptime
	}
	uptime, err := fn()
	if err != nil || uptime == 0 {
		return 0, false
	}
	return uptime, true
}

func shortUptime(sec uint64) string {
	d := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60
	if d > 0 {
		return fmt.Sprintf("%dd %dh", d, h)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%ds", sec)
}

type netCounters struct {
	RXBytes uint64
	TXBytes uint64
}

func (c *Collector) netRates(current netCounters, now time.Time) (float64, float64, bool) {
	c.mu.Lock()
	previous := c.netLast
	previousAt := c.netLastAt
	c.netLast = current
	c.netLastAt = now
	c.mu.Unlock()

	if previousAt.IsZero() || !now.After(previousAt) {
		return 0, 0, false
	}
	if current.RXBytes < previous.RXBytes || current.TXBytes < previous.TXBytes {
		return 0, 0, false
	}
	elapsed := now.Sub(previousAt).Seconds()
	if elapsed <= 0 {
		return 0, 0, false
	}
	downMbit := float64(current.RXBytes-previous.RXBytes) * 8 / 1024 / 1024 / elapsed
	upMbit := float64(current.TXBytes-previous.TXBytes) * 8 / 1024 / 1024 / elapsed
	return round2(downMbit), round2(upMbit), true
}

func readNetCounters(path string) (netCounters, bool) {
	if runtime.GOOS != "linux" && path == "" {
		return readPlatformNetCounters()
	}
	if path == "" {
		path = defaultNetDevPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return readPlatformNetCounters()
	}
	return parseProcNetDev(raw)
}

func readPlatformNetCounters() (netCounters, bool) {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("/usr/sbin/netstat", "-ibn").Output()
		if err != nil {
			return netCounters{}, false
		}
		return parseDarwinNetstat(out)
	}
	return netCounters{}, false
}

func parseDarwinNetstat(raw []byte) (netCounters, bool) {
	var total netCounters
	seen := false
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 11 || fields[0] == "Name" || !strings.HasPrefix(fields[2], "<Link#") {
			continue
		}
		iface := strings.TrimSuffix(fields[0], "*")
		if excludedNetInterface(iface) {
			continue
		}
		rx, rxErr := strconv.ParseUint(fields[6], 10, 64)
		tx, txErr := strconv.ParseUint(fields[9], 10, 64)
		if rxErr != nil || txErr != nil {
			continue
		}
		total.RXBytes += rx
		total.TXBytes += tx
		seen = true
	}
	return total, seen
}

func parseProcNetDev(raw []byte) (netCounters, bool) {
	var total netCounters
	seen := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		iface := strings.TrimSpace(parts[0])
		if excludedNetInterface(iface) {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 16 {
			continue
		}
		rx, rxErr := strconv.ParseUint(fields[0], 10, 64)
		tx, txErr := strconv.ParseUint(fields[8], 10, 64)
		if rxErr != nil || txErr != nil {
			continue
		}
		total.RXBytes += rx
		total.TXBytes += tx
		seen = true
	}
	return total, seen
}

func excludedNetInterface(name string) bool {
	if name == "lo" || name == "lo0" {
		return true
	}
	for _, prefix := range []string{
		"docker", "veth", "br-", "virbr", "tun", "tap", "wg", "zt", "tailscale",
		"bridge", "awdl", "llw", "utun", "gif", "stf", "p2p", "anpi",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func hostCPUPercent() (float64, bool) {
	if cpus, err := cpu.Percent(0, false); err == nil && len(cpus) > 0 {
		return round1(cpus[0]), true
	}
	if runtime.GOOS == "darwin" {
		if v, ok := darwinPSCPUPercent(); ok {
			return v, true
		}
		return darwinTopCPUPercent()
	}
	return 0, false
}

func darwinPSCPUPercent() (float64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-A", "-o", "%cpu=").Output()
	if err != nil {
		return 0, false
	}
	return parseDarwinPSCPUPercent(out, runtime.NumCPU())
}

func parseDarwinPSCPUPercent(out []byte, cores int) (float64, bool) {
	if cores <= 0 {
		cores = 1
	}
	total := 0.0
	count := 0
	for _, field := range strings.Fields(string(out)) {
		v, err := strconv.ParseFloat(field, 64)
		if err != nil {
			continue
		}
		total += v
		count++
	}
	if count == 0 {
		return 0, false
	}
	used := total / float64(cores)
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	return round1(used), true
}

func darwinTopCPUPercent() (float64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/top", "-l", "1", "-n", "0", "-s", "0").Output()
	if err != nil {
		return 0, false
	}
	return parseDarwinTopCPUPercent(out)
}

func parseDarwinTopCPUPercent(out []byte) (float64, bool) {
	m := darwinTopIdlePattern.FindSubmatch(out)
	if len(m) != 2 {
		return 0, false
	}
	idle, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil || idle < 0 || idle > 100 {
		return 0, false
	}
	return round1(100 - idle), true
}

func nodeProcessRSSBytes() int64 {
	out, err := exec.Command("pgrep", "-x", "quilibrium-node").Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0
	}
	rss, err := exec.Command("ps", "-o", "rss=", "-p", fields[0]).Output()
	if err != nil {
		return 0
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(rss)), 10, 64)
	if err != nil {
		return 0
	}
	return kb * 1024
}

// warmCPUStatic reads cpu model + core count once at startup. Cheap, but no
// reason to repeat per tick.
func (c *Collector) warmCPUStatic() {
	infos, err := cpu.Info()
	if err == nil && len(infos) > 0 {
		c.cpuModel = infos[0].ModelName
		c.cpuCores = 0
		for _, i := range infos {
			c.cpuCores += int(i.Cores)
		}
		if c.cpuCores == 0 {
			c.cpuCores = len(infos)
		}
		c.cpuStaticOK = true
	}
}

func (c *Collector) appendPoint(buf *[]float64, v float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	*buf = append(*buf, v)
	if len(*buf) > pointsCapacity {
		*buf = (*buf)[len(*buf)-pointsCapacity:]
	}
}

func (c *Collector) snapshotPoints(buf []float64) []float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(buf) == 0 {
		return nil
	}
	out := make([]float64, len(buf))
	copy(out, buf)
	return out
}

// round1 rounds to 1 decimal place — keeps frame payload compact and the UI
// from showing 32.4978236%.
func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// humanBytes formats bytes as "12.4 GB", with binary IEC units to match
// what most disk tools report.
func humanBytes(b uint64) string {
	const k = 1024.0
	f := float64(b)
	switch {
	case f < k:
		return fmt.Sprintf("%d B", b)
	case f < k*k:
		return fmt.Sprintf("%.1f KB", f/k)
	case f < k*k*k:
		return fmt.Sprintf("%.1f MB", f/(k*k))
	case f < k*k*k*k:
		return fmt.Sprintf("%.1f GB", f/(k*k*k))
	default:
		return fmt.Sprintf("%.1f TB", f/(k*k*k*k))
	}
}

// trimModel cuts marketing fluff like "Intel(R) Core(TM) i7-9750H @ 2.60GHz"
// down to a UI-friendly "i7-9750H @ 2.60GHz". Best-effort.
func trimModel(m string) string {
	m = strings.TrimSpace(m)
	if m == "" {
		return "CPU"
	}
	// drop common boilerplate
	for _, junk := range []string{"(R)", "(TM)", "Intel ", "AMD ", "Ryzen "} {
		m = strings.ReplaceAll(m, junk, "")
	}
	m = strings.Join(strings.Fields(m), " ")
	if len(m) > 40 {
		m = m[:40]
	}
	return m
}
