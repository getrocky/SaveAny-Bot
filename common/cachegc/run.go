package cachegc

import (
	"context"
	"time"

	"github.com/charmbracelet/log"
)

// Settings is the live configuration of the background cleaner. It is read
// again on every tick, so changes made in the web admin apply without a restart.
type Settings struct {
	Enabled bool
	// Dir is the resolved absolute cache directory; empty disables cleaning
	// (for example in stream mode or when the path is unsafe).
	Dir string
	// IdleFor is how long an entry must be untouched before a routine sweep removes it.
	IdleFor time.Duration
	// Interval is the time between routine sweeps.
	Interval time.Duration
	// LowDiskBytes triggers an aggressive sweep when free space drops below it; 0 disables.
	LowDiskBytes int64
}

const (
	// LowDiskIdle is the idle time required while free space is low.
	LowDiskIdle = 10 * time.Minute
	// StartupGrace is the time after start during which nothing is swept, so
	// tasks recovered at startup can claim their cache files first.
	StartupGrace = 3 * time.Minute
	// FirstSweepDelay is when the first routine sweep runs after start.
	FirstSweepDelay = 10 * time.Minute

	tick = time.Minute
)

// Run sweeps until ctx is cancelled. settings is called on every tick.
func Run(ctx context.Context, settings func() Settings, isActive func(name string) bool) {
	start := time.Now()
	var lastRoutine time.Time // zero until the first routine sweep
	var lastLowWarn time.Time
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s := settings()
			if !s.Enabled || s.Dir == "" || now.Sub(start) < StartupGrace {
				continue
			}
			interval := max(s.Interval, time.Minute)
			routine := false
			if lastRoutine.IsZero() {
				routine = now.Sub(start) >= FirstSweepDelay
			} else {
				routine = now.Sub(lastRoutine) >= interval
			}
			if routine {
				lastRoutine = now
				res, err := Sweep(Options{Dir: s.Dir, IdleFor: s.IdleFor, Now: now, IsActive: isActive})
				logResult("idle", s, res, err)
			}
			if s.LowDiskBytes > 0 {
				free := DiskFreeBytes(s.Dir)
				if free >= 0 && free < s.LowDiskBytes {
					res, err := Sweep(Options{Dir: s.Dir, IdleFor: LowDiskIdle, Now: now, IsActive: isActive})
					logResult("low-disk", s, res, err)
					if res.Removed == 0 && now.Sub(lastLowWarn) >= 30*time.Minute {
						lastLowWarn = now
						log.Warn("Disk space is low and nothing in the cache can be removed safely",
							"free_mb", free>>20, "threshold_mb", s.LowDiskBytes>>20, "kept", res.Kept)
					}
				}
			}
		}
	}
}

func logResult(kind string, s Settings, res Result, err error) {
	if err != nil {
		log.Error("Cache cleanup failed", "kind", kind, "error", err)
		return
	}
	if res.Removed > 0 || res.Errors > 0 {
		log.Info("Cache cleanup removed idle files", "kind", kind, "removed", res.Removed,
			"freed_mb", res.Bytes>>20, "kept", res.Kept, "errors", res.Errors, "dir", s.Dir)
	} else {
		log.Debug("Cache cleanup found nothing to remove", "kind", kind, "kept", res.Kept)
	}
}
