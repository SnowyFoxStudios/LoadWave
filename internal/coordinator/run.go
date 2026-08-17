// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package coordinator

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
	"github.com/SnowyFoxStudios/LoadWave/internal/metrics"
)

// maxRunEvents bounds the event log kept per run. Enough to explain what
// happened, bounded so a long run with a flapping agent cannot grow without
// limit.
const maxRunEvents = 500

// defaultGraceBudget mirrors the engine's default when a plan does not set one.
const defaultGraceBudget = 30 * time.Second

// Phase names as they appear in the API, chosen to read well in a UI rather
// than to mirror the protobuf enum's spelling.
const (
	PhasePending   = "pending"
	PhaseStarting  = "starting"
	PhaseRunning   = "running"
	PhaseStopping  = "stopping"
	PhaseCompleted = "completed"
	PhaseFailed    = "failed"
	PhaseAborted   = "aborted"
)

// phaseNames maps the wire enum onto API spellings.
var phaseNames = map[loadwavev1.RunPhase]string{
	loadwavev1.RunPhase_RUN_PHASE_PENDING:   PhasePending,
	loadwavev1.RunPhase_RUN_PHASE_STARTING:  PhaseStarting,
	loadwavev1.RunPhase_RUN_PHASE_RUNNING:   PhaseRunning,
	loadwavev1.RunPhase_RUN_PHASE_STOPPING:  PhaseStopping,
	loadwavev1.RunPhase_RUN_PHASE_COMPLETED: PhaseCompleted,
	loadwavev1.RunPhase_RUN_PHASE_FAILED:    PhaseFailed,
	loadwavev1.RunPhase_RUN_PHASE_ABORTED:   PhaseAborted,
}

// PhaseName renders a phase for the API.
func PhaseName(phase loadwavev1.RunPhase) string {
	if name, ok := phaseNames[phase]; ok {
		return name
	}
	return PhasePending
}

// IsTerminal reports whether a phase means the run is over.
func IsTerminal(phase loadwavev1.RunPhase) bool {
	switch phase {
	case loadwavev1.RunPhase_RUN_PHASE_COMPLETED,
		loadwavev1.RunPhase_RUN_PHASE_FAILED,
		loadwavev1.RunPhase_RUN_PHASE_ABORTED:
		return true
	default:
		return false
	}
}

// Event is something worth telling the operator about.
type Event struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Source  string            `json:"source"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// participant is one agent's involvement in a run.
type participant struct {
	AgentID    string `json:"agentId"`
	VUQuota    int    `json:"vuQuota"`
	RateQuota  int    `json:"rateQuota"`
	ShardIndex uint32 `json:"shardIndex"`
	Phase      string `json:"phase"`
	Message    string `json:"message,omitempty"`
	Dispatched bool   `json:"dispatched"`
}

// Run is the coordinator's record of one execution.
//
// Safe for concurrent use: the API serves it while control-stream goroutines
// update it.
type Run struct {
	mu sync.RWMutex

	id         string
	name       string
	plan       *loadwavev1.TestPlan
	store      *metrics.Store
	phase      loadwavev1.RunPhase
	createdAt  time.Time
	startAt    time.Time
	startedAt  time.Time
	stoppingAt time.Time
	endedAt    time.Time
	stopWhy    string
	failure    string

	peakVUs int
	// scaleRamp applies to the next dispatch only, so an operator's gradual
	// change is not re-applied every time the fleet changes shape.
	scaleRamp    time.Duration
	participants map[string]*participant
	events       []Event
	thresholds   []ThresholdResult
	breached     bool

	// published is the newest bucket already pushed to live subscribers, so
	// each tick sends only what is new.
	published time.Time
}

// newRun creates a pending run.
func newRun(id, name string, plan *loadwavev1.TestPlan, store *metrics.Store, peakVUs int) *Run {
	return &Run{
		id:           id,
		name:         name,
		plan:         plan,
		store:        store,
		phase:        loadwavev1.RunPhase_RUN_PHASE_PENDING,
		createdAt:    time.Now(),
		peakVUs:      peakVUs,
		participants: make(map[string]*participant),
	}
}

// ID returns the run's identifier.
func (r *Run) ID() string { return r.id }

// Store returns the run's metric store.
func (r *Run) Store() *metrics.Store { return r.store }

// Plan returns the plan being executed.
func (r *Run) Plan() *loadwavev1.TestPlan { return r.plan }

// Phase returns the current phase.
func (r *Run) Phase() loadwavev1.RunPhase {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.phase
}

// Active reports whether the run is still going.
func (r *Run) Active() bool { return !IsTerminal(r.Phase()) }

// setPhase advances the run, refusing to move on once it has finished.
//
// A run reaches a terminal phase exactly once. Without this guard a straggler
// agent reporting "completed" after an operator aborted the run would rewrite
// history, and the exit code with it.
func (r *Run) setPhase(phase loadwavev1.RunPhase, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if IsTerminal(r.phase) {
		return false
	}
	r.phase = phase

	switch {
	case phase == loadwavev1.RunPhase_RUN_PHASE_RUNNING && r.startedAt.IsZero():
		r.startedAt = time.Now()
	case phase == loadwavev1.RunPhase_RUN_PHASE_STOPPING && r.stoppingAt.IsZero():
		r.stoppingAt = time.Now()
	case IsTerminal(phase):
		r.endedAt = time.Now()
		r.stopWhy = reason
		if phase == loadwavev1.RunPhase_RUN_PHASE_FAILED {
			r.failure = reason
		}
	}
	return true
}

// addEvent appends to the run's event log, trimming the oldest entries.
func (r *Run) addEvent(event Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	r.events = append(r.events, event)
	if len(r.events) > maxRunEvents {
		// Copy forward rather than reslicing, so the trimmed entries become
		// garbage instead of being pinned by the backing array forever.
		keep := len(r.events) - maxRunEvents
		r.events = append(r.events[:0], r.events[keep:]...)
	}
}

// Events returns a copy of the run's event log.
func (r *Run) Events() []Event {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Event(nil), r.events...)
}

// setParticipants records the agents taking part.
func (r *Run) setParticipants(parts []*participant) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.participants = make(map[string]*participant, len(parts))
	for _, p := range parts {
		r.participants[p.AgentID] = p
	}
}

// updateParticipant records an agent's reported phase.
func (r *Run) updateParticipant(agentID, phase, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	p, ok := r.participants[agentID]
	if !ok {
		p = &participant{AgentID: agentID}
		r.participants[agentID] = p
	}
	p.Phase = phase
	p.Message = message
}

// Participants returns a copy of the agent roster.
func (r *Run) Participants() []participant {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]participant, 0, len(r.participants))
	for _, p := range r.participants {
		out = append(out, *p)
	}
	return out
}

// setThresholds records the latest threshold evaluation.
func (r *Run) setThresholds(results []ThresholdResult) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.thresholds = results
	for _, result := range results {
		if result.Evaluated && !result.Passed {
			r.breached = true
		}
	}
}

// StoppingFor reports how long the run has been trying to stop, or zero if it
// has not been asked to.
func (r *Run) StoppingFor() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.stoppingAt.IsZero() {
		return 0
	}
	return time.Since(r.stoppingAt)
}

// GraceBudget is how long the plan allows in-flight iterations to finish.
func (r *Run) GraceBudget() time.Duration {
	if grace := r.plan.GetLoad().GetGracefulStop().AsDuration(); grace > 0 {
		return grace
	}
	return defaultGraceBudget
}

// Thresholds returns the latest evaluation.
func (r *Run) Thresholds() []ThresholdResult {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]ThresholdResult(nil), r.thresholds...)
}

// Breached reports whether any threshold has failed at any point.
//
// It latches deliberately. A p95 that recovers by the end of the run still
// breached, and a CI gate that only looked at the final instant would let a
// genuine regression through.
func (r *Run) Breached() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.breached
}

// Summary is the API's view of a run.
type Summary struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Phase      string            `json:"phase"`
	CreatedAt  time.Time         `json:"createdAt"`
	StartAt    time.Time         `json:"startAt,omitempty"`
	StartedAt  time.Time         `json:"startedAt,omitempty"`
	EndedAt    time.Time         `json:"endedAt,omitempty"`
	ElapsedSec float64           `json:"elapsedSeconds"`
	PeakVUs    int               `json:"peakVUs"`
	Profile    string            `json:"profile"`
	BaseURL    string            `json:"baseURL"`
	StopReason string            `json:"stopReason,omitempty"`
	Failure    string            `json:"failure,omitempty"`
	Breached   bool              `json:"thresholdsBreached"`
	Tags       map[string]string `json:"tags,omitempty"`

	Participants []participant     `json:"participants"`
	Thresholds   []ThresholdResult `json:"thresholds"`
	Stats        metrics.Stats     `json:"stats"`
}

// Summary renders the run for the API.
func (r *Run) Summary(profile string) Summary {
	r.mu.RLock()
	participants := make([]participant, 0, len(r.participants))
	for _, p := range r.participants {
		participants = append(participants, *p)
	}
	summary := Summary{
		ID:           r.id,
		Name:         r.name,
		Phase:        PhaseName(r.phase),
		CreatedAt:    r.createdAt,
		StartAt:      r.startAt,
		StartedAt:    r.startedAt,
		EndedAt:      r.endedAt,
		PeakVUs:      r.peakVUs,
		Profile:      profile,
		BaseURL:      r.plan.GetBaseUrl(),
		StopReason:   r.stopWhy,
		Failure:      r.failure,
		Breached:     r.breached,
		Tags:         r.plan.GetTags(),
		Participants: participants,
		Thresholds:   append([]ThresholdResult(nil), r.thresholds...),
	}

	switch {
	case r.startedAt.IsZero():
	case r.endedAt.IsZero():
		summary.ElapsedSec = time.Since(r.startedAt).Seconds()
	default:
		summary.ElapsedSec = r.endedAt.Sub(r.startedAt).Seconds()
	}
	r.mu.RUnlock()

	summary.Stats = r.store.Stats()
	return summary
}

// NewRunID builds a sortable, human-legible run identifier.
//
// The timestamp prefix makes runs sort chronologically in a listing and in a
// directory of result files; the random suffix keeps two runs started in the
// same second apart.
func NewRunID(now time.Time) string {
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		// Only reachable if the system entropy source is broken, at which
		// point the timestamp alone is a reasonable fallback.
		return "run-" + now.UTC().Format("20060102-150405.000")
	}
	return fmt.Sprintf("run-%s-%s",
		now.UTC().Format("20060102-150405"), hex.EncodeToString(suffix[:]))
}

// sanitiseName reduces a user-supplied test name to something safe to embed
// in a filename or a URL path.
func sanitiseName(name string) string {
	if name == "" {
		return "loadwave"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	trimmed := strings.Trim(b.String(), "-")
	if trimmed == "" {
		return "loadwave"
	}
	if len(trimmed) > 64 {
		trimmed = trimmed[:64]
	}
	return trimmed
}
