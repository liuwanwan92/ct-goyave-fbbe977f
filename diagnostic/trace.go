package diagnostic

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"

	"goyave.dev/goyave/v5"
	goyaveSlog "goyave.dev/goyave/v5/slog"
)

// ExtraTraceID the key used in `Request.Extra` to store the trace ID.
type ExtraTraceID struct{}

// ExtraTraceLogger the key used in `Request.Extra` to store a logger
// that has the "trace_id" attribute baked in. Middleware and handlers can
// retrieve this logger to emit correlated log entries.
type ExtraTraceLogger struct{}

// DefaultHeaderName the default HTTP header name used to propagate the trace ID.
const DefaultHeaderName = "X-Trace-Id"

// traceCtxKey the private context key for the trace ID.
// Follows the existing pattern (see auth/authenticator.go:149, server.go:31).
type traceCtxKey struct{}

// Middleware generates or propagates a trace ID for each request.
// The trace ID is injected into:
//   - the request context (retrievable with `TraceIDFromContext`)
//   - `request.Extra[ExtraTraceID{}]`
//   - the response header (configurable, defaults to "X-Trace-Id")
//   - `request.Extra[ExtraTraceLogger{}]` — a `*slog.Logger` with "trace_id" attribute
//
// If `EnableStages` is true, a `*StageTracker` is also created in
// `request.Extra[ExtraStageTracker{}]`.
//
// The middleware intercepts panics to emit a correlated diagnostic log entry
// before re-panicking, ensuring the trace ID is captured even when the
// recovery middleware handles the panic response.
//
// This middleware should be registered as a global middleware. It will
// automatically execute after the built-in recovery and language middleware.
type Middleware struct {
	goyave.Component

	// HeaderName overrides the default header name ("X-Trace-Id").
	HeaderName string

	// EnableStages creates a StageTracker in request.Extra when true.
	EnableStages bool
}

var _ goyave.Middleware = (*Middleware)(nil)

// Handle implements `goyave.Middleware`.
func (m *Middleware) Handle(next goyave.Handler) goyave.Handler {
	return func(response *goyave.Response, request *goyave.Request) {
		headerName := m.HeaderName
		if headerName == "" {
			headerName = DefaultHeaderName
		}

		// Accept client-provided trace ID or generate one
		traceID := request.Header().Get(headerName)
		if traceID == "" {
			traceID = GenerateTraceID()
		}

		// Inject into all three propagation channels
		ctx := context.WithValue(request.Context(), traceCtxKey{}, traceID)
		request.WithContext(ctx)
		request.Extra[ExtraTraceID{}] = traceID
		response.Header().Set(headerName, traceID)

		// Create a trace-aware logger and store in Extra
		traceLogger := m.Logger().With(slog.String("trace_id", traceID))
		request.Extra[ExtraTraceLogger{}] = traceLogger

		if m.EnableStages {
			tracker := &StageTracker{}
			tracker.Record("request_received")
			request.Extra[ExtraStageTracker{}] = tracker
		}

		// Intercept panics for diagnostic correlation.
		// The recovery middleware (outermost) will still handle the 500 response,
		// but our intercept ensures a correlated diagnostic log entry exists
		// with the trace ID, enabling cross-layer troubleshooting.
		defer func() {
			if r := recover(); r != nil {
				traceLogger.ErrorCtx(
					request.Context(),
					fmt.Errorf("panic: %v", r),
					slog.String("diagnostic", "panic_intercepted"),
				)
				panic(r) // Re-panic for recovery middleware
			}
		}()

		next(response, request)
	}
}

// TraceIDFromContext extracts the trace ID from a `context.Context`.
// Returns "" if no trace ID is present.
func TraceIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(traceCtxKey{}).(string); ok {
		return id
	}
	return ""
}

// TraceIDFromRequest extracts the trace ID from a `*goyave.Request`'s Extra map.
// Returns "" if no trace ID is present.
func TraceIDFromRequest(request *goyave.Request) string {
	if id, ok := request.Extra[ExtraTraceID{}].(string); ok {
		return id
	}
	return ""
}

// TraceLoggerFromRequest retrieves the trace-aware logger from `request.Extra`.
// Returns nil if no trace logger is present (e.g., middleware not registered).
func TraceLoggerFromRequest(request *goyave.Request) *goyaveSlog.Logger {
	if logger, ok := request.Extra[ExtraTraceLogger{}].(*goyaveSlog.Logger); ok {
		return logger
	}
	return nil
}

// GenerateTraceID creates a 32-character hex string using `crypto/rand`.
// No external dependencies.
func GenerateTraceID() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		panic(fmt.Errorf("diagnostic: crypto/rand failed: %w", err))
	}
	return hex.EncodeToString(b)
}
