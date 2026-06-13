package database

import (
	"context"

	"gorm.io/gorm"
	"goyave.dev/goyave/v5/util/errors"
)

const (
	diagnosticCallbackBeforeName = "goyave:diagnostic_before"
	diagnosticCallbackAfterName  = "goyave:diagnostic_after"
)

// DiagnosticCollector collects diagnostic checkpoints for request correlation
// across layers (middleware, auth, database). Implementations must be safe
// for concurrent use.
type DiagnosticCollector interface {
	AddCheckpoint(layer, event string, err error)
}

type diagnosticCtxKey struct{}

// ContextWithDiagnosticCollector injects the given collector as a context value.
// The collector can be retrieved using DiagnosticCollectorFromContext.
func ContextWithDiagnosticCollector(ctx context.Context, c DiagnosticCollector) context.Context {
	return context.WithValue(ctx, diagnosticCtxKey{}, c)
}

// DiagnosticCollectorFromContext returns the DiagnosticCollector stored in the
// given context, or nil if none is present.
func DiagnosticCollectorFromContext(ctx context.Context) DiagnosticCollector {
	if c, ok := ctx.Value(diagnosticCtxKey{}).(DiagnosticCollector); ok {
		return c
	}
	return nil
}

// DiagnosticPlugin is a GORM plugin that records query start/end checkpoints
// into the DiagnosticCollector found in the statement's context.
// It is a no-op when no collector is present in the context.
type DiagnosticPlugin struct{}

// Name returns the name of the plugin.
func (p *DiagnosticPlugin) Name() string {
	return "goyave:diagnostic"
}

// Initialize registers before/after callbacks for all GORM operations.
func (p *DiagnosticPlugin) Initialize(db *gorm.DB) error {
	createCallback := db.Callback().Create()
	if err := createCallback.Before("*").Register(diagnosticCallbackBeforeName, p.diagnosticBefore); err != nil {
		return errors.New(err)
	}
	if err := createCallback.After("*").Register(diagnosticCallbackAfterName, p.diagnosticAfter); err != nil {
		return errors.New(err)
	}

	queryCallback := db.Callback().Query()
	if err := queryCallback.Before("*").Register(diagnosticCallbackBeforeName, p.diagnosticBefore); err != nil {
		return errors.New(err)
	}
	if err := queryCallback.After("*").Register(diagnosticCallbackAfterName, p.diagnosticAfter); err != nil {
		return errors.New(err)
	}

	deleteCallback := db.Callback().Delete()
	if err := deleteCallback.Before("*").Register(diagnosticCallbackBeforeName, p.diagnosticBefore); err != nil {
		return errors.New(err)
	}
	if err := deleteCallback.After("*").Register(diagnosticCallbackAfterName, p.diagnosticAfter); err != nil {
		return errors.New(err)
	}

	updateCallback := db.Callback().Update()
	if err := updateCallback.Before("*").Register(diagnosticCallbackBeforeName, p.diagnosticBefore); err != nil {
		return errors.New(err)
	}
	if err := updateCallback.After("*").Register(diagnosticCallbackAfterName, p.diagnosticAfter); err != nil {
		return errors.New(err)
	}

	rawCallback := db.Callback().Raw()
	if err := rawCallback.Before("*").Register(diagnosticCallbackBeforeName, p.diagnosticBefore); err != nil {
		return errors.New(err)
	}
	if err := rawCallback.After("*").Register(diagnosticCallbackAfterName, p.diagnosticAfter); err != nil {
		return errors.New(err)
	}

	return nil
}

func (p *DiagnosticPlugin) diagnosticBefore(db *gorm.DB) {
	if db.Statement.Context == nil {
		return
	}
	if c := DiagnosticCollectorFromContext(db.Statement.Context); c != nil {
		c.AddCheckpoint("db", "query_start", nil)
	}
}

func (p *DiagnosticPlugin) diagnosticAfter(db *gorm.DB) {
	if db.Statement.Context == nil {
		return
	}
	if c := DiagnosticCollectorFromContext(db.Statement.Context); c != nil {
		c.AddCheckpoint("db", "query_end", db.Error)
	}
}
