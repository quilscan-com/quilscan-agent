package telegramnotify

import (
	"context"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/diskinspect"
)

type sample struct {
	at          time.Time
	memory      float64
	hasMemory   bool
	rx, tx      uint64
	hasNetwork  bool
	nodeRunning bool
	nodeKnown   bool
	nodePID     int
}

type trafficPoint struct {
	at     time.Time
	rx, tx uint64
}

type trafficWindow struct{ points []trafficPoint }

const trafficDuration = 10 * time.Minute

func (w *trafficWindow) update(now time.Time, rx, tx uint64) (float64, float64, bool) {
	if len(w.points) > 0 {
		previous := w.points[len(w.points)-1]
		if !now.After(previous.at) || rx < previous.rx || tx < previous.tx {
			w.points = nil
		}
	}
	w.points = append(w.points, trafficPoint{now, rx, tx})
	start := now.Add(-trafficDuration)
	for len(w.points) > 1 && !w.points[1].at.After(start) {
		w.points = w.points[1:]
	}
	first := w.points[0]
	if len(w.points) < 2 || first.at.After(start) {
		return 0, 0, false
	}
	next := w.points[1]
	fraction := start.Sub(first.at).Seconds() / next.at.Sub(first.at).Seconds()
	down := float64(rx-first.rx) - fraction*float64(next.rx-first.rx)
	up := float64(tx-first.tx) - fraction*float64(next.tx-first.tx)
	toMbit := 8.0 / 1_000_000 / trafficDuration.Seconds()
	return down * toMbit, up * toMbit, true
}

func nodeConfigDisk(ctx context.Context, configFile string) (diskinspect.Usage, bool) {
	if configFile == "" {
		return diskinspect.Usage{}, false
	}
	usage, code := diskinspect.InspectFile(ctx, configFile)
	return usage, code == ""
}
