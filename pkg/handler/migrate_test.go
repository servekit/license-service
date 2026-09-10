package handler

import (
	"testing"

	"github.com/servekit/go-common/dbx"

	"github.com/servekit/license-service/internal/store/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrate_Idempotent verifies a second run on an already-migrated DB
// is a no-op.
func TestMigrate_Idempotent(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)

	require.NoError(t, Migrate(db))
	require.NoError(t, Migrate(db),
		"re-running migrate on a clean DB must not error")
}

// TestMigrate_Phase3TenantKeyBackfill: a pre-③ gate row (tenant_key NULL) is
// backfilled by Migrate — every app maps to its app_key literal (the ③
// window mapping; T10 总装 remaps to ten_* keys).
func TestMigrate_Phase3TenantKeyBackfill(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, Migrate(db)) // schema exists; no data to backfill yet

	// Seed a pre-③ row: NULL tenant_key is what pre-③ code wrote.
	require.NoError(t, db.Create(&models.LicenseApp{
		AppKey: "testkit", AppSecret: "s", Name: "testkit",
	}).Error)

	require.NoError(t, Migrate(db), "backfill run must succeed")

	var app models.LicenseApp
	require.NoError(t, db.Where("app_key = ?", "testkit").First(&app).Error)
	assert.Equal(t, "testkit", models.TenantKeyOf(app.TenantKey), "app maps to its app_key literal")

	// Third run: fully backfilled → reconcile passes, still idempotent.
	require.NoError(t, Migrate(db))
}

// TestMigrate_Phase3ReconcileAborts: a gate row that somehow keeps a NULL
// tenant_key after the backfill (e.g. an operator hand-NULLed it) must abort
// the migration loudly instead of silently drifting — the one-tenant-one-row
// unique index is only trustworthy when every row is mapped.
func TestMigrate_Phase3ReconcileAborts(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, Migrate(db))

	require.NoError(t, db.Create(&models.LicenseApp{
		AppKey: "ghost", AppSecret: "s", Name: "ghost",
	}).Error)
	// Simulate the unhealable state directly: backfill fixed nothing because
	// the row was inserted after it in the same run — force the drift.
	require.NoError(t, db.Exec(`UPDATE license_apps SET tenant_key = NULL WHERE app_key = 'ghost'`).Error)

	err := reconcileTenantKey(db, "license_apps",
		`SELECT count(*), count(tenant_key) FROM license_apps`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reconcile", "the failure must name the reconcile step")
}
