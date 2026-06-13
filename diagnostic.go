package goyave

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"goyave.dev/goyave/v5/database"
)

// ExtraDiagnostic is the key used to store the Diagnostic in Request.Extra.
type ExtraDiagnostic struct{}

// Checkpoint records a single diagnostic event in a request's lifecycle.
type Checkpoint struct {
	Layer     string
	Event     string
	Timestamp time.Time
	Error     string
}

// Diagnostic collects checkpoints across all layers for a single request.
// Safe for concurrent use.
type Diagnostic struct {
	CorrelationID string
	StartTime     time.Time
	mu            sync.Mutex
	Checkpoints   []Checkpoint
}

// AddCheckpoint records a diagnostic event. This method is safe for concurrent use.
func (d *Diagnostic) AddCheckpoint(layer, event string, err error) {
	cp := Checkpoint{
		Layer:     layer,
		Event:     event,
		Timestamp: time.Now(),
	}
	if err != nil {
		cp.Error = err.Error()
	}
	d.mu.Lock()
	d.Checkpoints = append(d.Checkpoints, cp)
	d.mu.Unlock()
}

var _ database.DiagnosticCollector = (*Diagnostic)(nil)

// DiagnosticMiddleware creates a Diagnostic for each request and propagates it
// through Request.Extra and the request context. It does not modify the response.
type DiagnosticMiddleware struct {
	Component
}

// Handle creates a Diagnostic, attaches it to the request, and calls next.
func (m *DiagnosticMiddleware) Handle(next Handler) Handler {
	return func(response *Response, request *Request) {
		now := time.Now()
		diag := &Diagnostic{
			CorrelationID: uuid.New().String(),
			StartTime:     now,
			Checkpoints:   make([]Checkpoint, 0, 8),
		}
		request.Extra[ExtraDiagnostic{}] = diag
		request.WithContext(database.ContextWithDiagnosticCollector(request.Context(), diag))
		diag.AddCheckpoint("middleware", "request_start", nil)
		next(response, request)
		diag.AddCheckpoint("middleware", "request_end", nil)
	}
}
