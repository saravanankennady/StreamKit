// Package heartbeat is how media workers tell the control plane they are alive. They report to the same table as
// media servers, so GET /servers shows both. It also runs the lease check: any worker that is alive can fail the jobs
// of one that is not.
package heartbeat

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/server/config"
)

const (
	// Interval is often enough that store.WorkerSeenWithin spots a dead worker quickly.
	Interval = 5 * time.Second
	// checkEvery spaces out health checks, which start processes and touch the disk.
	checkEvery = 15 * time.Second
	checkLimit = 8 * time.Second
	// slowChecksAllowed is how many checks in a row may run out of time before that counts against the worker.
	slowChecksAllowed = 3
)

// Stats are what a worker reports. The first group matches what media servers report, so one view can show both.
type Stats struct {
	NodeID        string  `json:"node_id"`
	Hostname      string  `json:"hostname"`
	CPUPercent    float64 `json:"cpu_percent"`
	NumCPU        int     `json:"num_cpu"`
	MemoryUsed    uint64  `json:"memory_used"`
	MemoryTotal   uint64  `json:"memory_total"`
	UptimeSeconds int64   `json:"uptime_seconds"`
	Draining      bool    `json:"draining"`
	Healthy       bool    `json:"healthy"`
	// UnhealthyReason says what is wrong, when Healthy is false.
	UnhealthyReason string `json:"unhealthy_reason,omitempty"`

	// Rooms and participants are always zero for a worker, and present so one view can show every row.
	ActiveRooms  int `json:"active_rooms"`
	Participants int `json:"participants"`

	Kind       string `json:"kind"`
	MaxJobs    int    `json:"max_jobs"`
	ActiveJobs int    `json:"active_jobs"`
}

// Reporter sends a worker's heartbeat.
type Reporter struct {
	Store   *store.Store
	Log     *slog.Logger
	NodeID  string
	Kind    string
	Region  string
	Address string
	MaxJobs int
	// Active counts running jobs.
	Active func() int
	// Check says whether this worker could do a job right now, and why not if it could not. A worker that fails it
	// takes no new work, and the API does not count it, so requests get a clear answer rather than a job that fails.
	Check func(ctx context.Context) error

	started  time.Time
	draining atomic.Bool
	leaving  atomic.Bool

	checkMu    sync.Mutex
	checkedAt  time.Time
	problem    string
	slowChecks int
}

// Healthy reports the result of the last health check. A worker that has not run one yet has not been shown to be broken.
func (r *Reporter) Healthy() bool {
	r.checkMu.Lock()
	defer r.checkMu.Unlock()
	return r.problem == ""
}

// checkHealth runs the check if it is due, and returns what is wrong, if anything. A check that runs out of time says the
// machine is slow, not that the worker is broken, so that only counts when it keeps happening.
func (r *Reporter) checkHealth(ctx context.Context) string {
	r.checkMu.Lock()
	due := r.Check != nil && (r.checkedAt.IsZero() || time.Since(r.checkedAt) >= checkEvery)
	problem := r.problem
	r.checkMu.Unlock()
	if !due {
		return problem
	}
	checkCtx, cancel := context.WithTimeout(ctx, checkLimit)
	defer cancel()
	result := ""
	if err := r.Check(checkCtx); err != nil {
		result = err.Error()
	}
	if ctx.Err() != nil {
		return problem // Shutting down says nothing about the worker's health.
	}
	timedOut := result != "" && errors.Is(checkCtx.Err(), context.DeadlineExceeded)

	r.checkMu.Lock()
	defer r.checkMu.Unlock()
	r.checkedAt = time.Now()
	if timedOut {
		r.slowChecks++
		if r.slowChecks < slowChecksAllowed {
			r.Log.Warn("a health check ran out of time", "problem", result)
			return r.problem
		}
	} else {
		r.slowChecks = 0
	}
	if result != r.problem {
		if result != "" {
			r.Log.Error("this worker cannot take jobs", "problem", result)
		} else {
			r.Log.Info("this worker is healthy again")
		}
	}
	r.problem = result
	return result
}

// Leave reports no capacity from now on, for a worker finishing its jobs before it exits.
func (r *Reporter) Leave(ctx context.Context) {
	r.leaving.Store(true)
	r.beat(ctx)
}

// Draining reports whether an operator has asked this worker to take no more work.
func (r *Reporter) Draining() bool { return r.draining.Load() }

// Run reports until ctx ends. The first report goes out before it returns control to the timer, so a worker that has
// just started can take jobs straight away.
func (r *Reporter) Run(ctx context.Context) {
	r.started = time.Now()
	t := time.NewTicker(Interval)
	defer t.Stop()
	r.beat(ctx)
	for {
		select {
		case <-ctx.Done():
			r.goodbye()
			return
		case <-t.C:
			r.beat(ctx)
		}
	}
}

// goodbye marks this worker as gone at once, rather than leaving the API to offer it for another few seconds.
func (r *Reporter) goodbye() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := r.Store.DB.Exec(ctx, `UPDATE servers SET last_seen_at = now() - interval '1 hour' WHERE node_id = $1`, r.NodeID); err != nil {
		r.Log.Warn("could not report shutdown", "error", err)
	}
}

func (r *Reporter) beat(ctx context.Context) {
	hostname, _ := os.Hostname()
	problem := r.checkHealth(ctx)
	s := Stats{
		NodeID: r.NodeID, Hostname: hostname, NumCPU: runtime.NumCPU(), UptimeSeconds: int64(time.Since(r.started).Seconds()),
		Draining: r.draining.Load(), Healthy: problem == "", UnhealthyReason: problem, Kind: r.Kind, MaxJobs: r.MaxJobs, ActiveJobs: r.Active(),
	}
	if p, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(p) > 0 {
		s.CPUPercent = p[0]
	}
	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		s.MemoryUsed, s.MemoryTotal = v.Used, v.Total
	}
	if r.leaving.Load() {
		s.MaxJobs, s.Draining = 0, true
	}
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	draining, err := r.Store.HeartbeatServer(ctx, store.ServerNode{
		NodeID: r.NodeID, Region: r.Region, Version: config.Version, Address: r.Address, StartedAt: r.started, Stats: data,
	})
	if err != nil {
		if ctx.Err() == nil {
			r.Log.Warn("heartbeat failed", "error", err)
		}
		return
	}
	r.draining.Store(draining)
}

// Reap runs the lease check until ctx ends.
func Reap(ctx context.Context, st *store.Store, log *slog.Logger) {
	st.ReapLoop(ctx, Interval, log, nil)
}
