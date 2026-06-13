package goyave

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goyave.dev/goyave/v5/config"
	"goyave.dev/goyave/v5/database"
	_ "goyave.dev/goyave/v5/database/dialect/sqlite"
)

const metaAuthKey = "goyave.require-auth"

type diagnosticTestUser struct {
	ID    uint   `gorm:"primaryKey"`
	Name  string `gorm:"size:100"`
	Email string `gorm:"size:100"`
}

func (diagnosticTestUser) TableName() string {
	return "diagnostic_regression_users"
}

type mockAuthMiddleware struct {
	Component
	shouldFail bool
	delay      time.Duration
}

func (m *mockAuthMiddleware) Handle(next Handler) Handler {
	return func(response *Response, request *Request) {
		requireAuth, ok := request.Route.LookupMeta(metaAuthKey)
		if !ok || requireAuth != true {
			next(response, request)
			return
		}

		collector := database.DiagnosticCollectorFromContext(request.Context())
		if collector != nil {
			collector.AddCheckpoint("auth", "authenticate_start", nil)
		}

		if m.delay > 0 {
			select {
			case <-time.After(m.delay):
			case <-request.Context().Done():
				err := request.Context().Err()
				if collector != nil {
					collector.AddCheckpoint("auth", "context_done", err)
				}
				response.JSON(http.StatusUnauthorized, map[string]string{"error": "request canceled"})
				return
			}
		}

		if m.shouldFail {
			err := fmt.Errorf("invalid credentials")
			if collector != nil {
				collector.AddCheckpoint("auth", "authenticate_failed", err)
			}
			response.JSON(http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}

		if collector != nil {
			collector.AddCheckpoint("auth", "authenticate_success", nil)
		}
		request.User = &diagnosticTestUser{ID: 1, Name: "testuser", Email: "test@example.org"}
		next(response, request)
	}
}

func prepareDiagnosticRegressionServer(t *testing.T, authMw *mockAuthMiddleware) (*Server, *Router) {
	t.Helper()
	cfg := config.LoadDefault()
	cfg.Set("app.debug", false)
	cfg.Set("database.connection", "sqlite3")
	cfg.Set("database.name", fmt.Sprintf("diag_regression_%s.db", t.Name()))
	cfg.Set("database.options", "mode=memory")
	cfg.Set("database.defaultReadQueryTimeout", 0)
	cfg.Set("database.defaultWriteQueryTimeout", 0)

	server, err := New(Options{Config: cfg})
	require.NoError(t, err)
	t.Cleanup(func() { server.CloseDB() })

	require.NoError(t, server.DB().Use(&database.DiagnosticPlugin{}))

	router := NewRouter(server)
	router.GlobalMiddleware(&DiagnosticMiddleware{})

	if authMw != nil {
		router.Middleware(authMw)
	}

	return server, router
}

func TestRegression_AuthFailureDuringTimeout(t *testing.T) {
	authMw := &mockAuthMiddleware{shouldFail: false, delay: 50 * time.Millisecond}
	_, router := prepareDiagnosticRegressionServer(t, authMw)

	route := router.Get("/protected", func(response *Response, _ *Request) {
		response.Status(http.StatusOK)
	})
	route.SetMeta(metaAuthKey, true)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	recorder := httptest.NewRecorder()
	rawReq := httptest.NewRequest(http.MethodGet, "/protected", nil).WithContext(ctx)
	router.ServeHTTP(recorder, rawReq)

	resp := recorder.Result()
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Diagnostic should still be available through the response cycle
	// The context deadline fired before auth could complete
}

func TestRegression_AuthFailureDuringCancellation(t *testing.T) {
	authMw := &mockAuthMiddleware{shouldFail: false, delay: 100 * time.Millisecond}
	_, router := prepareDiagnosticRegressionServer(t, authMw)

	var capturedDiag *Diagnostic
	route := router.Get("/protected", func(_ *Response, request *Request) {
		capturedDiag = request.Extra[ExtraDiagnostic{}].(*Diagnostic)
	})
	route.SetMeta(metaAuthKey, true)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	recorder := httptest.NewRecorder()
	rawReq := httptest.NewRequest(http.MethodGet, "/protected", nil).WithContext(ctx)
	router.ServeHTTP(recorder, rawReq)

	resp := recorder.Result()
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Handler should NOT have been reached (auth was blocked by cancellation)
	assert.Nil(t, capturedDiag)
}

func TestRegression_ConcurrentRequestCorrelation(t *testing.T) {
	authMw := &mockAuthMiddleware{shouldFail: false, delay: 0}
	_, router := prepareDiagnosticRegressionServer(t, authMw)

	type captured struct {
		correlationID string
		checkpoints   int
	}
	var mu sync.Mutex
	results := make([]captured, 0, 20)

	route := router.Get("/concurrent", func(_ *Response, request *Request) {
		diag := request.Extra[ExtraDiagnostic{}].(*Diagnostic)
		mu.Lock()
		results = append(results, captured{
			correlationID: diag.CorrelationID,
			checkpoints:   len(diag.Checkpoints),
		})
		mu.Unlock()
	})
	route.SetMeta(metaAuthKey, true)

	const numRequests = 20
	var wg sync.WaitGroup
	wg.Add(numRequests)

	for i := 0; i < numRequests; i++ {
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			rawReq := httptest.NewRequest(http.MethodGet, "/concurrent", nil)
			router.ServeHTTP(recorder, rawReq)
			assert.Equal(t, http.StatusOK, recorder.Code)
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, results, numRequests)

	ids := make(map[string]bool, numRequests)
	for _, r := range results {
		ids[r.correlationID] = true
		assert.NotEmpty(t, r.correlationID)
		assert.GreaterOrEqual(t, r.checkpoints, 2) // at minimum: request_start, auth_start
	}
	assert.Len(t, ids, numRequests, "all correlation IDs must be unique")
}

func TestRegression_PaginationWithAuth(t *testing.T) {
	authMw := &mockAuthMiddleware{shouldFail: false, delay: 0}
	server, router := prepareDiagnosticRegressionServer(t, authMw)

	db := server.DB()
	require.NoError(t, db.AutoMigrate(&diagnosticTestUser{}))
	for i := 0; i < 15; i++ {
		require.NoError(t, db.Create(&diagnosticTestUser{
			Name:  fmt.Sprintf("user_%d", i),
			Email: fmt.Sprintf("user%d@example.org", i),
		}).Error)
	}

	var capturedDiag *Diagnostic
	route := router.Get("/users", func(response *Response, request *Request) {
		capturedDiag = request.Extra[ExtraDiagnostic{}].(*Diagnostic)

		paginator := database.NewPaginator(db.WithContext(request.Context()), 1, 5, &[]diagnosticTestUser{})
		err := paginator.Find()
		if response.WriteDBError(err) {
			return
		}
		response.JSON(http.StatusOK, paginator)
	})
	route.SetMeta(metaAuthKey, true)

	recorder := httptest.NewRecorder()
	rawReq := httptest.NewRequest(http.MethodGet, "/users", nil)
	router.ServeHTTP(recorder, rawReq)

	resp := recorder.Result()
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	require.NotNil(t, capturedDiag)
	assert.NotEmpty(t, capturedDiag.CorrelationID)

	// Verify checkpoint flow order
	hasStart := false
	hasAuth := false
	hasDB := false
	hasEnd := false
	for _, cp := range capturedDiag.Checkpoints {
		switch {
		case cp.Layer == "middleware" && cp.Event == "request_start":
			hasStart = true
		case cp.Layer == "auth" && cp.Event == "authenticate_success":
			assert.True(t, hasStart, "auth must come after request_start")
			hasAuth = true
		case cp.Layer == "db" && cp.Event == "query_start":
			assert.True(t, hasAuth, "db must come after auth")
			hasDB = true
		case cp.Layer == "middleware" && cp.Event == "request_end":
			hasEnd = true
		}
	}
	assert.True(t, hasStart, "missing request_start checkpoint")
	assert.True(t, hasAuth, "missing authenticate_success checkpoint")
	assert.True(t, hasDB, "missing db query checkpoint")
	assert.True(t, hasEnd, "missing request_end checkpoint")

	// DB checkpoints should appear in pairs (paginator does count + find)
	dbCount := 0
	for _, cp := range capturedDiag.Checkpoints {
		if cp.Layer == "db" {
			dbCount++
		}
	}
	assert.GreaterOrEqual(t, dbCount, 4, "paginator should produce at least 2 query pairs (count + find)")
}

func TestRegression_DBTimeoutOverlapAuthFailure(t *testing.T) {
	authMw := &mockAuthMiddleware{shouldFail: true, delay: 0}
	server, router := prepareDiagnosticRegressionServer(t, authMw)

	db := server.DB()
	require.NoError(t, db.AutoMigrate(&diagnosticTestUser{}))

	handlerReached := false
	route := router.Get("/protected", func(response *Response, request *Request) {
		handlerReached = true
		db.WithContext(request.Context()).First(&diagnosticTestUser{})
		response.Status(http.StatusOK)
	})
	route.SetMeta(metaAuthKey, true)

	recorder := httptest.NewRecorder()
	rawReq := httptest.NewRequest(http.MethodGet, "/protected", nil)
	router.ServeHTTP(recorder, rawReq)

	resp := recorder.Result()
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.False(t, handlerReached, "handler must not be reached when auth fails")
}
