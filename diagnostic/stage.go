package diagnostic

import "sync"

// ExtraStageTracker the key used in `Request.Extra` to store the StageTracker.
type ExtraStageTracker struct{}

// StageTracker records an ordered list of stages a request passes through.
// Safe for concurrent use.
//
// This is a test/troubleshooting diagnostic tool. It is only created when
// `Middleware.EnableStages` is true, so it adds zero overhead by default.
type StageTracker struct {
	mu     sync.Mutex
	stages []string
}

// Record appends a stage name to the tracker.
func (t *StageTracker) Record(stage string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stages = append(t.stages, stage)
}

// Stages returns a copy of the recorded stages.
func (t *StageTracker) Stages() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	cpy := make([]string, len(t.stages))
	copy(cpy, t.stages)
	return cpy
}
