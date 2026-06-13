package testutil

import (
	"testing"

	"goyave.dev/goyave/v5"
)

// DiagnosticFromRequest extracts the Diagnostic from the request's Extra map.
// Returns nil if no diagnostic middleware was active for this request.
func DiagnosticFromRequest(request *goyave.Request) *goyave.Diagnostic {
	if d, ok := request.Extra[goyave.ExtraDiagnostic{}].(*goyave.Diagnostic); ok {
		return d
	}
	return nil
}

// AssertHasCheckpoint asserts that the diagnostic contains at least one checkpoint
// matching the given layer and event.
func AssertHasCheckpoint(t *testing.T, diag *goyave.Diagnostic, layer, event string) bool {
	t.Helper()
	if diag == nil {
		t.Errorf("diagnostic is nil, expected checkpoint %s/%s", layer, event)
		return false
	}
	for _, cp := range diag.Checkpoints {
		if cp.Layer == layer && cp.Event == event {
			return true
		}
	}
	t.Errorf("checkpoint %s/%s not found in diagnostic (correlation=%s, checkpoints=%d)",
		layer, event, diag.CorrelationID, len(diag.Checkpoints))
	return false
}

// AssertCheckpointOrder asserts that checkpoints with the given event names
// appear in the specified order within the diagnostic. Events not listed are ignored.
func AssertCheckpointOrder(t *testing.T, diag *goyave.Diagnostic, events ...string) bool {
	t.Helper()
	if diag == nil {
		t.Errorf("diagnostic is nil, expected checkpoint order %v", events)
		return false
	}
	idx := 0
	for _, cp := range diag.Checkpoints {
		if idx < len(events) && cp.Event == events[idx] {
			idx++
		}
	}
	if idx < len(events) {
		t.Errorf("checkpoint order mismatch: expected %v in order, but only matched %d/%d (correlation=%s)",
			events, idx, len(events), diag.CorrelationID)
		return false
	}
	return true
}

// CountCheckpoints returns the number of checkpoints for the given layer.
func CountCheckpoints(diag *goyave.Diagnostic, layer string) int {
	if diag == nil {
		return 0
	}
	count := 0
	for _, cp := range diag.Checkpoints {
		if cp.Layer == layer {
			count++
		}
	}
	return count
}
