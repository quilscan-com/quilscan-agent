// Package metrics samples host + node-process resource usage and pushes a
// `metrics` frame to backend on a fixed cadence. The frame shape mirrors what
// the my-nodes page reads from agentSnapshot.metrics — see frontend
// composables/useAgent.js for the consumer side.
package metrics

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/nodeinfo"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
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

	// NodeMetricsURL is the loopback Prometheus endpoint exposed by the node
	// binary when invoked with --prometheus-server. We scrape just two gauges
	// from it: process_resident_memory_bytes and
	// quilibrium_global_consensus_current_frame_number.
	// Empty string disables node-level metrics.
	NodeMetricsURL string
	// DiskPath is the partition whose usage we report (default "/").
	DiskPath string

	// UnitName is the platform service unit of the quilibrium-node
	// (e.g. "quilibrium-node.service" on Linux, "com.quilscan.node" on
	// macOS). When non-empty and Svc is set, we probe IsActive each tick
	// to surface a `node_running` bool — distinguishes "installed but
	// stopped" from "running" without waiting on prometheus scrape success.
	UnitName string
	// NodeLogPath is set on macOS where launchd redirects node logs to a file.
	// Linux leaves it empty and the collector falls back to journalctl.
	NodeLogPath string

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
	streaming   bool
	modeCh      chan struct{}

	lastRuntimeRead time.Time
	runtimeStatus   *nodeinfo.RuntimeStatus
}

const (
	pointsCapacity = 24 // ~ 1 minute of data at 3s tick
)

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

	if cpus, err := cpu.Percent(0, false); err == nil && len(cpus) > 0 {
		v := round1(cpus[0])
		frame["cpu"] = v
		c.appendPoint(&c.cpuPoints, v)
	}
	if c.cpuStaticOK {
		frame["cpu_sub"] = fmt.Sprintf("%d Core · %s", c.cpuCores, trimModel(c.cpuModel))
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

	// Node-level metrics come from the node's own Prometheus endpoint
	// (enabled by the service definition's --prometheus-server flag). We only
	// care about two gauges, so a tiny line-by-line parser is enough.
	gotNodeMem := false
	gotNodeFrame := false
	if c.NodeMetricsURL != "" {
		if vals, err := scrapeNodeMetrics(c.NodeMetricsURL); err == nil {
			if vals.residentMemBytes > 0 {
				frame["node_mem_bytes"] = vals.residentMemBytes
				frame["node_mem_sub"] = humanBytes(uint64(vals.residentMemBytes))
				gotNodeMem = true
			}
			if vals.frameNumber > 0 {
				frame["node_frame_height"] = vals.frameNumber
				gotNodeFrame = true
			}
		}
	}
	if !gotNodeFrame {
		if status := c.runtimeStatusFromLogs(); status != nil && status.FrameNumber > 0 {
			frame["node_frame_height"] = status.FrameNumber
		}
	}
	if !gotNodeMem {
		if rss := nodeProcessRSSBytes(); rss > 0 {
			frame["node_mem_bytes"] = rss
			frame["node_mem_sub"] = humanBytes(uint64(rss))
		}
	}

	if c.UnitName != "" && c.Svc != nil {
		frame["node_running"] = c.Svc.IsActive(c.UnitName)
	}

	if !c.Started.IsZero() {
		frame["uptime_sec"] = int64(time.Since(c.Started).Seconds())
	}

	return frame
}

func (c *Collector) runtimeStatusFromLogs() *nodeinfo.RuntimeStatus {
	if time.Since(c.lastRuntimeRead) < 30*time.Second {
		return c.runtimeStatus
	}
	c.lastRuntimeRead = time.Now()
	if c.NodeLogPath != "" {
		c.runtimeStatus = nodeinfo.RuntimeStatusFromLogFile(context.Background(), c.NodeLogPath, 1000, 5*time.Second)
		return c.runtimeStatus
	}
	if c.UnitName != "" {
		c.runtimeStatus = nodeinfo.RuntimeStatusFromJournal(context.Background(), c.UnitName, 1000, 5*time.Second)
		return c.runtimeStatus
	}
	return nil
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

// scrapedNodeValues collects only the two Prometheus gauges this collector
// forwards to backend. Anything else in the /metrics dump is intentionally
// ignored — the wire payload stays small and we don't need to track schema
// drift across node versions.
type scrapedNodeValues struct {
	residentMemBytes int64
	frameNumber      int64
}

// scrapeNodeMetrics fetches the node's /metrics endpoint and pulls just two
// gauge values. Robust against the file being huge: we read line-by-line and
// stop after both fields are filled.
func scrapeNodeMetrics(url string) (scrapedNodeValues, error) {
	var v scrapedNodeValues
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return v, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		// Drain & discard so the connection can be reused
		_, _ = io.Copy(io.Discard, resp.Body)
		return v, fmt.Errorf("http %d", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	gotMem, gotFrame := false, false
	for sc.Scan() {
		line := sc.Text()
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		switch {
		case !gotMem && strings.HasPrefix(line, "process_resident_memory_bytes"):
			if val, ok := parsePromValue(line); ok {
				v.residentMemBytes = int64(val)
				gotMem = true
			}
		case !gotFrame && strings.HasPrefix(line, "quilibrium_global_consensus_current_frame_number"):
			if val, ok := parsePromValue(line); ok {
				v.frameNumber = int64(val)
				gotFrame = true
			}
		}
		if gotMem && gotFrame {
			break
		}
	}
	return v, sc.Err()
}

// parsePromValue extracts the numeric value from a Prometheus exposition
// line: "metric_name{labels} 12345.678" or "metric_name 12345.678". Returns
// false if the line doesn't fit the shape.
func parsePromValue(line string) (float64, bool) {
	// Skip optional labels block
	if idx := strings.IndexByte(line, '}'); idx >= 0 {
		line = line[idx+1:]
	} else if idx := strings.IndexByte(line, ' '); idx >= 0 {
		line = line[idx+1:]
	} else {
		return 0, false
	}
	line = strings.TrimSpace(line)
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		line = line[:i]
	}
	if line == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(line, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// round1 rounds to 1 decimal place — keeps frame payload compact and the UI
// from showing 32.4978236%.
func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
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
