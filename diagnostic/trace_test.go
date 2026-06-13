package diagnostic

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateTraceID(t *testing.T) {
	id := GenerateTraceID()
	assert.Len(t, id, 32, "trace ID should be 32 hex characters")

	// Must be valid hex
	_, err := hex.DecodeString(id)
	require.NoError(t, err, "trace ID must be valid hex")

	// Must be unique
	id2 := GenerateTraceID()
	assert.NotEqual(t, id, id2, "two generated trace IDs must differ")
}

func TestTraceIDFromContext(t *testing.T) {
	t.Run("empty context", func(t *testing.T) {
		assert.Empty(t, TraceIDFromContext(context.Background()))
	})

	t.Run("with trace ID", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), traceCtxKey{}, "abc123")
		assert.Equal(t, "abc123", TraceIDFromContext(ctx))
	})

	t.Run("wrong type", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), traceCtxKey{}, 42)
		assert.Empty(t, TraceIDFromContext(ctx))
	})
}

func TestStageTracker(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		tracker := &StageTracker{}
		assert.Empty(t, tracker.Stages())

		tracker.Record("stage1")
		tracker.Record("stage2")
		assert.Equal(t, []string{"stage1", "stage2"}, tracker.Stages())
	})

	t.Run("returns copy", func(t *testing.T) {
		tracker := &StageTracker{}
		tracker.Record("original")

		stages := tracker.Stages()
		stages[0] = "modified"
		assert.Equal(t, "original", tracker.Stages()[0],
			"Stages() must return a copy, not a reference")
	})
}

func TestStageTracker_Concurrent(t *testing.T) {
	tracker := &StageTracker{}
	const goroutines = 100
	done := make(chan struct{}, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			tracker.Record("stage")
			done <- struct{}{}
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}

	assert.Len(t, tracker.Stages(), goroutines,
		"no stages should be lost under concurrency")
}
