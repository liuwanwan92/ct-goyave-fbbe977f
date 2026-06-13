package goyave

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goyave.dev/goyave/v5/config"
	"goyave.dev/goyave/v5/database"
)

func TestDiagnosticCheckpoint(t *testing.T) {
	t.Run("AddCheckpoint", func(t *testing.T) {
		diag := &Diagnostic{
			CorrelationID: "test-id",
			Checkpoints:   make([]Checkpoint, 0, 4),
		}
		diag.AddCheckpoint("middleware", "request_start", nil)
		diag.AddCheckpoint("auth", "authenticate", nil)
		diag.AddCheckpoint("db", "query_end", assert.AnError)

		require.Len(t, diag.Checkpoints, 3)
		assert.Equal(t, "middleware", diag.Checkpoints[0].Layer)
		assert.Equal(t, "request_start", diag.Checkpoints[0].Event)
		assert.Empty(t, diag.Checkpoints[0].Error)
		assert.False(t, diag.Checkpoints[0].Timestamp.IsZero())

		assert.Equal(t, "auth", diag.Checkpoints[1].Layer)
		assert.Equal(t, "authenticate", diag.Checkpoints[1].Event)

		assert.Equal(t, "db", diag.Checkpoints[2].Layer)
		assert.Equal(t, "query_end", diag.Checkpoints[2].Event)
		assert.Equal(t, assert.AnError.Error(), diag.Checkpoints[2].Error)
	})

	t.Run("ConcurrentSafety", func(t *testing.T) {
		diag := &Diagnostic{
			CorrelationID: "concurrent-test",
			Checkpoints:   make([]Checkpoint, 0, 100),
		}

		const goroutines = 20
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				diag.AddCheckpoint("test", "event", nil)
			}()
		}
		wg.Wait()

		assert.Len(t, diag.Checkpoints, goroutines)
	})

	t.Run("ImplementsCollector", func(t *testing.T) {
		var _ database.DiagnosticCollector = (*Diagnostic)(nil)
	})
}

func TestDiagnosticMiddleware(t *testing.T) {
	t.Run("AttachesDiagnostic", func(t *testing.T) {
		cfg := config.LoadDefault()
		server, err := New(Options{Config: cfg})
		require.NoError(t, err)

		middleware := &DiagnosticMiddleware{}
		middleware.Init(server)

		var captured *Diagnostic
		handler := middleware.Handle(func(_ *Response, req *Request) {
			d, ok := req.Extra[ExtraDiagnostic{}].(*Diagnostic)
			require.True(t, ok)
			captured = d

			ctxCollector := database.DiagnosticCollectorFromContext(req.Context())
			assert.Same(t, d, ctxCollector)
		})

		request := NewRequest(httptest.NewRequest(http.MethodGet, "/test", nil))
		response := NewResponse(server, request, httptest.NewRecorder())
		handler(response, request)

		require.NotNil(t, captured)
		assert.NotEmpty(t, captured.CorrelationID)
		assert.False(t, captured.StartTime.IsZero())
		require.GreaterOrEqual(t, len(captured.Checkpoints), 2)
		assert.Equal(t, "middleware", captured.Checkpoints[0].Layer)
		assert.Equal(t, "request_start", captured.Checkpoints[0].Event)
		assert.Equal(t, "middleware", captured.Checkpoints[len(captured.Checkpoints)-1].Layer)
		assert.Equal(t, "request_end", captured.Checkpoints[len(captured.Checkpoints)-1].Event)
	})

	t.Run("UniqueCorrelationIDs", func(t *testing.T) {
		cfg := config.LoadDefault()
		server, err := New(Options{Config: cfg})
		require.NoError(t, err)

		middleware := &DiagnosticMiddleware{}
		middleware.Init(server)

		ids := make(map[string]bool)
		handler := middleware.Handle(func(_ *Response, req *Request) {
			d := req.Extra[ExtraDiagnostic{}].(*Diagnostic)
			ids[d.CorrelationID] = true
		})

		for i := 0; i < 10; i++ {
			request := NewRequest(httptest.NewRequest(http.MethodGet, "/test", nil))
			response := NewResponse(server, request, httptest.NewRecorder())
			handler(response, request)
		}

		assert.Len(t, ids, 10)
	})

	t.Run("DoesNotModifyResponse", func(t *testing.T) {
		cfg := config.LoadDefault()
		server, err := New(Options{Config: cfg})
		require.NoError(t, err)

		middleware := &DiagnosticMiddleware{}
		middleware.Init(server)

		handler := middleware.Handle(func(resp *Response, _ *Request) {
			resp.Status(http.StatusTeapot)
		})

		request := NewRequest(httptest.NewRequest(http.MethodGet, "/test", nil))
		recorder := httptest.NewRecorder()
		response := NewResponse(server, request, recorder)
		handler(response, request)

		assert.Equal(t, http.StatusTeapot, response.status)
	})

	t.Run("OptIn", func(t *testing.T) {
		request := NewRequest(httptest.NewRequest(http.MethodGet, "/test", nil))
		_, exists := request.Extra[ExtraDiagnostic{}]
		assert.False(t, exists)

		ctx := request.Context()
		assert.Nil(t, database.DiagnosticCollectorFromContext(ctx))
	})
}
