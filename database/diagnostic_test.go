package database

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"goyave.dev/goyave/v5/config"
)

type mockCheckpoint struct {
	layer string
	event string
	err   error
}

type mockDiagnosticCollector struct {
	mu          sync.Mutex
	checkpoints []mockCheckpoint
}

func (m *mockDiagnosticCollector) AddCheckpoint(layer, event string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkpoints = append(m.checkpoints, mockCheckpoint{layer, event, err})
}

func (m *mockDiagnosticCollector) getCheckpoints() []mockCheckpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]mockCheckpoint, len(m.checkpoints))
	copy(cp, m.checkpoints)
	return cp
}

func TestDiagnosticPlugin(t *testing.T) {
	RegisterDialect("sqlite3_diagnostic_test", "file:{name}?{options}", sqlite.Open)
	t.Cleanup(func() {
		mu.Lock()
		delete(dialects, "sqlite3_diagnostic_test")
		mu.Unlock()
	})

	t.Run("Name", func(t *testing.T) {
		plugin := &DiagnosticPlugin{}
		assert.Equal(t, "goyave:diagnostic", plugin.Name())
	})

	t.Run("Callbacks", func(t *testing.T) {
		db := prepareDiagnosticTestDB(t)
		callbacks := db.Callback()

		assert.NotNil(t, callbacks.Create().Get(diagnosticCallbackBeforeName))
		assert.NotNil(t, callbacks.Create().Get(diagnosticCallbackAfterName))
		assert.NotNil(t, callbacks.Query().Get(diagnosticCallbackBeforeName))
		assert.NotNil(t, callbacks.Query().Get(diagnosticCallbackAfterName))
		assert.NotNil(t, callbacks.Delete().Get(diagnosticCallbackBeforeName))
		assert.NotNil(t, callbacks.Delete().Get(diagnosticCallbackAfterName))
		assert.NotNil(t, callbacks.Update().Get(diagnosticCallbackBeforeName))
		assert.NotNil(t, callbacks.Update().Get(diagnosticCallbackAfterName))
		assert.NotNil(t, callbacks.Raw().Get(diagnosticCallbackBeforeName))
		assert.NotNil(t, callbacks.Raw().Get(diagnosticCallbackAfterName))
	})

	t.Run("PluginRegistered", func(t *testing.T) {
		db := prepareDiagnosticTestDB(t)
		plugin, ok := db.Plugins[(&DiagnosticPlugin{}).Name()]
		assert.True(t, ok)
		_, ok = plugin.(*DiagnosticPlugin)
		assert.True(t, ok)
	})

	t.Run("CheckpointsRecorded", func(t *testing.T) {
		db := prepareDiagnosticTestDB(t)
		db.AutoMigrate(&diagnosticTestModel{})

		collector := &mockDiagnosticCollector{}
		ctx := ContextWithDiagnosticCollector(context.Background(), collector)

		db.WithContext(ctx).Create(&diagnosticTestModel{Name: "test"})

		cps := collector.getCheckpoints()
		require.GreaterOrEqual(t, len(cps), 2)
		assert.Equal(t, "db", cps[0].layer)
		assert.Equal(t, "query_start", cps[0].event)
		assert.Nil(t, cps[0].err)
		assert.Equal(t, "db", cps[len(cps)-1].layer)
		assert.Equal(t, "query_end", cps[len(cps)-1].event)
		assert.Nil(t, cps[len(cps)-1].err)
	})

	t.Run("NoCollectorInContext", func(t *testing.T) {
		db := prepareDiagnosticTestDB(t)
		db.AutoMigrate(&diagnosticTestModel{})

		res := db.WithContext(context.Background()).Create(&diagnosticTestModel{Name: "safe"})
		assert.NoError(t, res.Error)
	})

	t.Run("ErrorRecorded", func(t *testing.T) {
		db := prepareDiagnosticTestDB(t)

		collector := &mockDiagnosticCollector{}
		ctx := ContextWithDiagnosticCollector(context.Background(), collector)

		var result diagnosticTestModel
		db.WithContext(ctx).Table("nonexistent_table").First(&result)

		cps := collector.getCheckpoints()
		require.GreaterOrEqual(t, len(cps), 2)
		lastCp := cps[len(cps)-1]
		assert.Equal(t, "query_end", lastCp.event)
		assert.NotNil(t, lastCp.err)
	})
}

func TestDiagnosticCollectorContext(t *testing.T) {
	t.Run("RoundTrip", func(t *testing.T) {
		collector := &mockDiagnosticCollector{}
		ctx := ContextWithDiagnosticCollector(context.Background(), collector)
		got := DiagnosticCollectorFromContext(ctx)
		assert.Same(t, collector, got)
	})

	t.Run("NilContext", func(t *testing.T) {
		got := DiagnosticCollectorFromContext(context.Background())
		assert.Nil(t, got)
	})
}

type diagnosticTestModel struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"size:100"`
}

func (diagnosticTestModel) TableName() string {
	return "diagnostic_test_models"
}

func prepareDiagnosticTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	cfg := config.LoadDefault()
	cfg.Set("database.connection", "sqlite3_diagnostic_test")
	cfg.Set("database.name", fmt.Sprintf("diagnostic_test_%s.db", t.Name()))
	cfg.Set("database.options", "mode=memory")
	cfg.Set("database.defaultReadQueryTimeout", 0)
	cfg.Set("database.defaultWriteQueryTimeout", 0)

	db, err := New(cfg, nil)
	require.NoError(t, err)

	require.NoError(t, db.Use(&DiagnosticPlugin{}))

	t.Cleanup(func() {
		sqlDB, _ := db.DB()
		if sqlDB != nil {
			sqlDB.Close()
		}
	})

	return db
}
