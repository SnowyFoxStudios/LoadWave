// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package idspace partitions the virtual user id range across a cluster.
//
// Every virtual user in a run needs an id that is unique fleet-wide: ids seed
// each VU's random source, pick its slice of shared fixtures, and identify it
// in logs. Asking a central allocator for one would put a network round trip
// in the middle of scaling up. Instead the range is carved statically — the
// coordinator gives each agent a block, the agent subdivides it for its
// workers — so allocation is a local increment and collisions are impossible
// by construction.
package idspace

// Block sizes. Generous, because the space is 63 bits wide and the only cost
// of a large block is that ids are not densely packed: at these strides a run
// can span roughly nine hundred thousand agents before it exhausts the range,
// which is comfortably more than anyone will need.
const (
	// PerWorker is how many ids each worker process may allocate.
	PerWorker int64 = 100_000

	// PerAgent is how many ids each agent may hand out across its workers,
	// allowing up to a hundred workers per agent.
	PerAgent int64 = PerWorker * 100
)

// AgentBase returns the first id belonging to the agent at the given index.
func AgentBase(agentIndex int) int64 {
	if agentIndex < 0 {
		agentIndex = 0
	}
	return int64(agentIndex) * PerAgent
}

// WorkerBase returns the first id belonging to one worker of an agent.
//
// A worker index beyond the block wraps rather than spilling into the next
// agent's range. Overlapping ids inside one agent would be a nuisance; ids
// colliding across hosts would corrupt data sharding, so the wrap keeps the
// damage local and bounded.
func WorkerBase(agentBase int64, workerIndex int) int64 {
	if workerIndex < 0 {
		workerIndex = 0
	}
	offset := (int64(workerIndex) * PerWorker) % PerAgent
	return agentBase + offset
}
