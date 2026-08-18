// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package procstats reports a process's own resource usage — CPU time and
// resident memory — for nodes to include in their heartbeats. Neither figure
// is available from the Go standard library: runtime.MemStats describes the
// Go heap, not the process's actual memory footprint, and there is no
// standard-library equivalent of CPU accounting at all. Both need an
// OS-specific query, which is what this package delegates to gopsutil for.
package procstats

import (
	"os"

	"github.com/shirou/gopsutil/v4/process"
)

// Self measures the current process's own resource usage.
type Self struct {
	proc *process.Process
}

// NewSelf prepares a reader for the current process.
//
// Never fails: if the underlying OS handle cannot be opened, Usage simply
// reports zero from then on. A heartbeat missing this detail is far less
// costly than plumbing a constructor error through for something this minor.
func NewSelf() *Self {
	proc, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		return &Self{}
	}
	return &Self{proc: proc}
}

// Usage reports this process's CPU use — a percentage averaged over its
// whole life so far, where 100 means one full core sustained — and its
// resident memory in bytes. Either figure comes back zero if the underlying
// OS query fails, which is treated as "unknown right now" rather than an
// error worth surfacing.
func (s *Self) Usage() (cpuPercent float64, memBytes uint64) {
	if s.proc == nil {
		return 0, 0
	}
	if percent, err := s.proc.CPUPercent(); err == nil {
		cpuPercent = percent
	}
	if mem, err := s.proc.MemoryInfo(); err == nil && mem != nil {
		memBytes = mem.RSS
	}
	return cpuPercent, memBytes
}
