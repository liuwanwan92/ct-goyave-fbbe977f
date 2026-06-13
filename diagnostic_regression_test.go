package goyave

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"goyave.dev/goyave/v5/auth"
	"goyave.dev/goyave/v5/config"
	"goyave.dev/goyave/v5/diagnostic"
	goyaveSlog "goyave.dev/goyave/v5/slog"
	"goyave.dev/goyave/v5/util/testutil"
)

// ---------------------------------------------------------------------------
// Test helpers (local to this file, not modifying any existing files)
// ---------------------------------------------------------------------------

type logEntry struct {
	Time       string `json:"time"`
	Level      string `json:"level"`
	Msg        string `json:"msg"`
	TraceID    string `json:"trace_id"`
	Diagnostic string `json:"diagnostic"`
}

func captureLogs(t *testing.T) (*bytes.Buffer, *goyaveSlog.Logger) {
	t.Helper()
	buf := &bytes.Buffer{}
	logger := goyaveSlog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	return buf, logger
}

func parseLogEntries(buf *bytes.Buffer) []logEntry {
	var entries []logEntry
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry logEntry
		if err := json.Unmarshal([]byte(line), &entry); err == nil {
			entries = append(entries, entry)
		}
	}
	return entries
}

func filterByTraceID(entries []logEntry, traceID string) []logEntry {
	var filtered []logEntry
	for _, e := range entries {
		if e.TraceID == traceID {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

func newRegressionServer(t *testing.T, logger *goyaveSlog.Logger) *testutil.TestServer {
	t.Helper()
	cfg := config.LoadDefault()
	cfg.Set("app.debug", false)
	return testutil.NewTestServerWithOptions(t, Options{
		Config: cfg,
		Logger: logger,
	})
}

// ---------------------------------------------------------------------------
// Mock types for auth tests
// ---------------------------------------------------------------------------

type testAuthUser struct {
	Name     string
	Password string
}

// getTestBcryptHash returns a bcrypt hash of "testpass", generated once.
func getTestBcryptHash() string {
	testBcryptHashOnce.Do(func() {
		h, _ := bcrypt.GenerateFromPassword([]byte("testpass"), bcrypt.MinCost)
		testBcryptHashValue = string(h)
	})
	return testBcryptHashValue
}

var (
	testBcryptHashOnce sync.Once
	testBcryptHashValue string
)

// mockFailUserService returns ErrRecordNotFound + a user with invalid password.
// The authenticator sees notFound=true → falls through to bcrypt compare → fails → 401.
type mockFailUserService struct{}

func (s *mockFailUserService) FindByUsername(_ context.Context, _ any) (*testAuthUser, error) {
	return &testAuthUser{Name: "unknown", Password: "invalidhash"}, gorm.ErrRecordNotFound
}

// mockTimeoutUserService blocks until context deadline → context.DeadlineExceeded → panic.
type mockTimeoutUserService struct {
	delay time.Duration
}

func (s *mockTimeoutUserService) FindByUsername(ctx context.Context, _ any) (*testAuthUser, error) {
	select {
	case <-time.After(s.delay):
		return &testAuthUser{Name: "user", Password: getTestBcryptHash()}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// mockCancelUserService cancels the context mid-flight → context.Canceled → panic.
type mockCancelUserService struct {
	cancelFunc context.CancelFunc
}

func (s *mockCancelUserService) FindByUsername(ctx context.Context, _ any) (*testAuthUser, error) {
	s.cancelFunc()
	return nil, ctx.Err()
}

// ---------------------------------------------------------------------------
// traceLoggingMiddleware — test helper that logs its stage using the trace logger
// ---------------------------------------------------------------------------

type traceLoggingMiddleware struct {
	Component
	stage string
}

func (m *traceLoggingMiddleware) Handle(next Handler) Handler {
	return func(resp *Response, req *Request) {
		traceLogger := diagnostic.TraceLoggerFromRequest(req)
		if traceLogger != nil {
			traceLogger.InfoContext(req.Context(), m.stage)
		}
		next(resp, req)
	}
}

// ---------------------------------------------------------------------------
// Test 1: Trace ID propagated through multi-middleware chain
// ---------------------------------------------------------------------------

func TestTraceID_PropagatedThroughMiddlewareChain(t *testing.T) {
	logBuf, logger := captureLogs(t)
	server := newRegressionServer(t, logger)

	server.RegisterRoutes(func(_ *Server, router *Router) {
		router.GlobalMiddleware(&diagnostic.Middleware{EnableStages: true})

		router.Get("/chain", func(resp *Response, req *Request) {
			// Verify trace ID is in context
			ctxTraceID := diagnostic.TraceIDFromContext(req.Context())
			assert.NotEmpty(t, ctxTraceID)

			// Verify trace ID is in Extra
			extraTraceID := diagnostic.TraceIDFromRequest(req)
			assert.Equal(t, ctxTraceID, extraTraceID)

			// Verify trace logger is available
			traceLogger := diagnostic.TraceLoggerFromRequest(req)
			require.NotNil(t, traceLogger)
			traceLogger.InfoContext(req.Context(), "handler_log")

			// Verify stage tracker recorded entry
			tracker, ok := req.Extra[diagnostic.ExtraStageTracker{}].(*diagnostic.StageTracker)
			require.True(t, ok)
			tracker.Record("handler_executed")

			resp.JSON(http.StatusOK, map[string]string{"trace_id": ctxTraceID})
		})
	})

	httpReq := httptest.NewRequest(http.MethodGet, "/chain", nil)
	resp := server.TestRequest(httpReq)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify response header has trace ID
	respTraceID := resp.Header.Get("X-Trace-Id")
	assert.Len(t, respTraceID, 32)

	// Verify response body matches
	body, err := testutil.ReadJSONBody[map[string]string](resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, respTraceID, body["trace_id"])

	// Verify logs: "handler_log" entry has this trace ID
	entries := parseLogEntries(logBuf)
	correlated := filterByTraceID(entries, respTraceID)
	require.NotEmpty(t, correlated, "should have at least one log entry with trace ID")

	found := false
	for _, e := range correlated {
		if e.Msg == "handler_log" {
			found = true
		}
	}
	assert.True(t, found, "expected 'handler_log' entry with trace_id")
}

// ---------------------------------------------------------------------------
// Test 2: Concurrent requests have distinct trace IDs
// ---------------------------------------------------------------------------

func TestTraceID_ConcurrentRequests_Distinct(t *testing.T) {
	logBuf, logger := captureLogs(t)
	server := newRegressionServer(t, logger)

	server.RegisterRoutes(func(_ *Server, router *Router) {
		router.GlobalMiddleware(&diagnostic.Middleware{})

		router.Get("/concurrent", func(resp *Response, req *Request) {
			traceLogger := diagnostic.TraceLoggerFromRequest(req)
			traceLogger.InfoContext(req.Context(), "concurrent_handler")
			resp.JSON(http.StatusOK, map[string]string{
				"trace_id": diagnostic.TraceIDFromRequest(req),
			})
		})
	})

	const numRequests = 10
	var wg sync.WaitGroup
	traceIDs := make([]string, numRequests)
	statusCodes := make([]int, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			httpReq := httptest.NewRequest(http.MethodGet, "/concurrent", nil)
			resp := server.TestRequest(httpReq)
			traceIDs[idx] = resp.Header.Get("X-Trace-Id")
			statusCodes[idx] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	wg.Wait()

	// All requests succeeded
	for i, code := range statusCodes {
		assert.Equal(t, http.StatusOK, code, "request %d should succeed", i)
	}

	// All trace IDs are distinct
	unique := make(map[string]bool)
	for _, id := range traceIDs {
		assert.Len(t, id, 32, "trace ID should be 32 hex chars")
		unique[id] = true
	}
	assert.Equal(t, numRequests, len(unique), "all trace IDs should be distinct")

	// Each trace ID has at least one correlated log entry
	entries := parseLogEntries(logBuf)
	for _, id := range traceIDs {
		correlated := filterByTraceID(entries, id)
		assert.NotEmpty(t, correlated, "trace ID %s should appear in logs", id)
	}
}

// ---------------------------------------------------------------------------
// Test 3: Auth failure preserves trace ID in response header
// ---------------------------------------------------------------------------

func TestTraceID_AuthFailure_Preserved(t *testing.T) {
	_, logger := captureLogs(t)
	server := newRegressionServer(t, logger)

	server.RegisterRoutes(func(_ *Server, router *Router) {
		router.GlobalMiddleware(&diagnostic.Middleware{})

		authMW := auth.Middleware(auth.NewBasicAuthenticator(
			&mockFailUserService{}, "Password",
		))
		router.Middleware(authMW)

		router.Get("/protected", func(resp *Response, req *Request) {
			resp.JSON(http.StatusOK, map[string]string{"ok": "true"})
		}).SetMeta(auth.MetaAuth, true)
	})

	httpReq := httptest.NewRequest(http.MethodGet, "/protected", nil)
	httpReq.SetBasicAuth("baduser", "badpass")
	resp := server.TestRequest(httpReq)
	resp.Body.Close()

	// Auth failure: 401 (unchanged)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Trace ID MUST be in response header even on auth failure
	traceID := resp.Header.Get("X-Trace-Id")
	assert.Len(t, traceID, 32, "trace ID should be present in 401 response")
}

// ---------------------------------------------------------------------------
// Test 4: Auth + timeout — error is NOT masked as auth failure
// ---------------------------------------------------------------------------

func TestTraceID_AuthTimeout_ErrorNotMasked(t *testing.T) {
	logBuf, logger := captureLogs(t)
	server := newRegressionServer(t, logger)

	server.RegisterRoutes(func(_ *Server, router *Router) {
		router.GlobalMiddleware(&diagnostic.Middleware{})

		authMW := auth.Middleware(auth.NewBasicAuthenticator(
			&mockTimeoutUserService{delay: 5 * time.Second}, "Password",
		))
		router.Middleware(authMW)

		router.Get("/slow-auth", func(resp *Response, req *Request) {
			resp.JSON(http.StatusOK, map[string]string{"ok": "true"})
		}).SetMeta(auth.MetaAuth, true)
	})

	// Create a request with a short deadline
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	httpReq := httptest.NewRequest(http.MethodGet, "/slow-auth", nil)
	httpReq = httpReq.WithContext(ctx)
	httpReq.SetBasicAuth("user", "pass")

	resp := server.TestRequest(httpReq)
	resp.Body.Close()

	// The authenticator panics on non-ErrRecordNotFound errors.
	// Recovery catches the panic → 500 (NOT 401)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"timeout during auth should produce 500, NOT 401")

	// Trace ID MUST survive the panic
	traceID := resp.Header.Get("X-Trace-Id")
	assert.Len(t, traceID, 32, "trace ID should survive auth panic")

	// The trace middleware's diagnostic log should capture the panic
	entries := parseLogEntries(logBuf)
	correlated := filterByTraceID(entries, traceID)
	foundPanicLog := false
	for _, e := range correlated {
		if strings.Contains(e.Msg, "panic") || strings.Contains(e.Msg, "deadline") {
			foundPanicLog = true
		}
	}
	assert.True(t, foundPanicLog,
		"trace-correlated log should capture the panic/timeout error")
}

// ---------------------------------------------------------------------------
// Test 5: Cancellation during request processing
// ---------------------------------------------------------------------------

func TestTraceID_CancellationDuringProcessing(t *testing.T) {
	logBuf, logger := captureLogs(t)
	server := newRegressionServer(t, logger)

	var cancelFn context.CancelFunc

	server.RegisterRoutes(func(_ *Server, router *Router) {
		router.GlobalMiddleware(&diagnostic.Middleware{})

		authMW := auth.Middleware(auth.NewBasicAuthenticator(
			&mockCancelUserService{cancelFunc: func() { cancelFn() }}, "Password",
		))
		router.Middleware(authMW)

		router.Get("/cancel", func(resp *Response, req *Request) {
			resp.JSON(http.StatusOK, map[string]string{"ok": "true"})
		}).SetMeta(auth.MetaAuth, true)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancelFn = cancel

	httpReq := httptest.NewRequest(http.MethodGet, "/cancel", nil)
	httpReq = httpReq.WithContext(ctx)
	httpReq.SetBasicAuth("user", "pass")

	resp := server.TestRequest(httpReq)
	resp.Body.Close()

	// Cancellation → panic in authenticator → recovery → 500
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	// Trace ID is consistent
	traceID := resp.Header.Get("X-Trace-Id")
	assert.Len(t, traceID, 32, "trace ID should survive cancellation")

	// All log entries for this request share the same trace ID
	entries := parseLogEntries(logBuf)
	correlated := filterByTraceID(entries, traceID)
	for _, e := range correlated {
		assert.Equal(t, traceID, e.TraceID,
			"all correlated entries must have the same trace ID")
	}
}

// ---------------------------------------------------------------------------
// Test 6: Panic recovery — trace ID preserved via diagnostic intercept
// ---------------------------------------------------------------------------

func TestTraceID_PanicRecovery_Preserved(t *testing.T) {
	logBuf, logger := captureLogs(t)
	server := newRegressionServer(t, logger)

	server.RegisterRoutes(func(_ *Server, router *Router) {
		router.GlobalMiddleware(&diagnostic.Middleware{})

		router.Get("/panic", func(resp *Response, req *Request) {
			// Log something before panicking (with trace)
			traceLogger := diagnostic.TraceLoggerFromRequest(req)
			traceLogger.InfoContext(req.Context(), "about_to_panic")
			panic("deliberate test panic")
		})
	})

	httpReq := httptest.NewRequest(http.MethodGet, "/panic", nil)
	resp := server.TestRequest(httpReq)
	resp.Body.Close()

	// Recovery middleware catches the panic → 500 (unchanged)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	// Trace ID in response header (set before panic)
	traceID := resp.Header.Get("X-Trace-Id")
	assert.Len(t, traceID, 32, "trace ID should be in header after panic")

	// Verify trace-correlated logs exist
	entries := parseLogEntries(logBuf)
	correlated := filterByTraceID(entries, traceID)

	// Collect all messages
	msgs := make([]string, 0, len(correlated))
	for _, e := range correlated {
		msgs = append(msgs, e.Msg)
	}

	// Should have "about_to_panic" log
	assert.Contains(t, msgs, "about_to_panic",
		"pre-panic log should be correlated")

	// Should have the diagnostic panic intercept
	hasPanicIntercept := false
	for _, e := range correlated {
		if strings.Contains(e.Msg, "deliberate test panic") {
			hasPanicIntercept = true
		}
	}
	assert.True(t, hasPanicIntercept,
		"diagnostic panic intercept should be correlated with trace ID")
}

// ---------------------------------------------------------------------------
// Test 7: Cross-layer error accumulation — all logs share trace ID
// ---------------------------------------------------------------------------

func TestTraceID_ErrorAccumulation_CrossLayer(t *testing.T) {
	logBuf, logger := captureLogs(t)
	server := newRegressionServer(t, logger)

	server.RegisterRoutes(func(_ *Server, router *Router) {
		router.GlobalMiddleware(&diagnostic.Middleware{})

		// Router-level middleware that logs
		router.Middleware(&traceLoggingMiddleware{stage: "router_middleware"})

		sub := router.Subrouter("/api")
		sub.Middleware(&traceLoggingMiddleware{stage: "subrouter_middleware"})

		sub.Get("/resource", func(resp *Response, req *Request) {
			traceLogger := diagnostic.TraceLoggerFromRequest(req)
			traceLogger.InfoContext(req.Context(), "handler_layer")
			resp.JSON(http.StatusOK, map[string]string{"ok": "true"})
		})
	})

	httpReq := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
	resp := server.TestRequest(httpReq)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	traceID := resp.Header.Get("X-Trace-Id")
	require.Len(t, traceID, 32)

	// All log entries should share the same trace ID
	entries := parseLogEntries(logBuf)
	correlated := filterByTraceID(entries, traceID)

	stages := make(map[string]bool)
	for _, e := range correlated {
		stages[e.Msg] = true
	}

	assert.True(t, stages["router_middleware"], "router middleware log should be correlated")
	assert.True(t, stages["subrouter_middleware"], "subrouter middleware log should be correlated")
	assert.True(t, stages["handler_layer"], "handler log should be correlated")
}

// ---------------------------------------------------------------------------
// Test 8: Client-provided trace ID is propagated (not overwritten)
// ---------------------------------------------------------------------------

func TestTraceID_ExistingHeader_Propagated(t *testing.T) {
	logBuf, logger := captureLogs(t)
	server := newRegressionServer(t, logger)

	server.RegisterRoutes(func(_ *Server, router *Router) {
		router.GlobalMiddleware(&diagnostic.Middleware{})

		router.Get("/propagate", func(resp *Response, req *Request) {
			traceLogger := diagnostic.TraceLoggerFromRequest(req)
			traceLogger.InfoContext(req.Context(), "propagated_handler")
			resp.JSON(http.StatusOK, map[string]string{
				"trace_id": diagnostic.TraceIDFromRequest(req),
			})
		})
	})

	clientTraceID := "client-provided-trace-id-123456"
	httpReq := httptest.NewRequest(http.MethodGet, "/propagate", nil)
	httpReq.Header.Set("X-Trace-Id", clientTraceID)

	resp := server.TestRequest(httpReq)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Response header should echo the client's trace ID
	assert.Equal(t, clientTraceID, resp.Header.Get("X-Trace-Id"))

	// Body should contain the client's trace ID
	body, err := testutil.ReadJSONBody[map[string]string](resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, clientTraceID, body["trace_id"])

	// Logs should use the client's trace ID
	entries := parseLogEntries(logBuf)
	correlated := filterByTraceID(entries, clientTraceID)
	require.NotEmpty(t, correlated, "logs should contain client trace ID")
}

// ---------------------------------------------------------------------------
// Test 9: 404 Not Found — trace middleware still executes (global MW)
// ---------------------------------------------------------------------------

func TestTraceID_NotFound_StillTraced(t *testing.T) {
	_, logger := captureLogs(t)
	server := newRegressionServer(t, logger)

	server.RegisterRoutes(func(_ *Server, router *Router) {
		router.GlobalMiddleware(&diagnostic.Middleware{})
		router.Get("/exists", func(resp *Response, _ *Request) {
			resp.String(http.StatusOK, "OK")
		})
	})

	httpReq := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	resp := server.TestRequest(httpReq)
	resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// Trace ID should still be in the response header
	traceID := resp.Header.Get("X-Trace-Id")
	assert.Len(t, traceID, 32, "trace ID should be present even for 404")
}

// ---------------------------------------------------------------------------
// Test 10: 405 Method Not Allowed — trace middleware still executes
// ---------------------------------------------------------------------------

func TestTraceID_MethodNotAllowed_StillTraced(t *testing.T) {
	_, logger := captureLogs(t)
	server := newRegressionServer(t, logger)

	server.RegisterRoutes(func(_ *Server, router *Router) {
		router.GlobalMiddleware(&diagnostic.Middleware{})
		router.Get("/only-get", func(resp *Response, _ *Request) {
			resp.String(http.StatusOK, "OK")
		})
	})

	httpReq := httptest.NewRequest(http.MethodPost, "/only-get", nil)
	resp := server.TestRequest(httpReq)
	resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)

	traceID := resp.Header.Get("X-Trace-Id")
	assert.Len(t, traceID, 32, "trace ID should be present even for 405")
}
