package handler

import (
	"fmt"
	"log/slog"

	"github.com/servekit/go-common/dbx"
	"gorm.io/gorm"

	"github.com/servekit/license-service/internal/store/models"
)

// Migrate applies the current schema to db via GORM AutoMigrate, then runs
// the phase ③ tenant_key post-migration (backfill + reconcile — the same
// procedure as deploy/phase3-license-tenant-key.sql, so `make migrate` alone
// re-keys a pre-③ database; fresh DBs no-op).
//
// This is the single migration entry point for license-service: the `migrate`
// subcommand (cmd/server) and embedders that inject a parent db
// (pkg.NewModule + option.WithDB) both call it, so tables are created
// regardless of how the service runs. pkg re-exports it as pkg.Migrate.
//
// AutoMigrate creates missing tables/columns/indexes but never drops unused
// ones — when a column is removed from a model, dev DBs are recreated via
// testcontainer rather than migrated in place.
//
// Embedders migrate on the parent db before constructing the module:
//
//	licensepkg.Migrate(parentDB)
//	hdl, err := licensepkg.NewModule(cfg, option.WithDB(parentDB))
func Migrate(db *gorm.DB) error {
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		return fmt.Errorf("auto-migrate: %w", err)
	}
	if err := postMigrateTenantKey(db); err != nil {
		return fmt.Errorf("post-migrate tenant_key: %w", err)
	}
	if err := postMigrateDropLegacy(db); err != nil {
		return fmt.Errorf("post-migrate legacy drops: %w", err)
	}
	return nil
}

// postMigrateTenantKey mirrors deploy/phase3-license-tenant-key.sql after
// AutoMigrate has added the column and the uq_license_apps_tenant_key index
// (D-③4: add → backfill → reconcile). license_apps is the ONLY table
// touched: the gate row is license-service's single per-tenant artifact —
// keys/devices/trials/entitlements are global (no app/tenant dimension), so
// there is no superseded composite index to drop. Rows written by pre-③ code
// during the deploy window are healed by the next run's backfill.
func postMigrateTenantKey(db *gorm.DB) error {
	// QF1008 false positive: Dialector is an interface-typed field, Name is
	// its method — the selector cannot be removed.
	//nolint:staticcheck // gorm.DB.Dialector is an interface field, not embedding
	if db.Dialector.Name() != "postgres" {
		// Non-PG dev dialects (sqlite testcontainers are PG here; MySQL
		// deployments run the deploy SQL) — indexes already come from
		// AutoMigrate; nothing to backfill on a fresh DB.
		return nil
	}

	// Backfill (idempotent: NULL rows only). Every gate row maps to its
	// app_key literal (the ③ window mapping; T10 总装 remaps to ten_*).
	res := db.Exec(`UPDATE license_apps SET tenant_key = app_key WHERE tenant_key IS NULL`)
	if res.Error != nil {
		return fmt.Errorf("backfill license_apps: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		slog.Info("phase3 license tenant_key backfill", "license_apps", res.RowsAffected)
	}

	// Reconcile: every gate row carries a tenant mapping. A row that stays
	// NULL after the backfill means something outside the model wrote it —
	// fail loudly so the operator resolves it instead of silently drifting.
	return reconcileTenantKey(db, "license_apps",
		`SELECT count(*), count(tenant_key) FROM license_apps`)
}

// reconcileTenantKey asserts that total == filled for the given probe and
// names the table and step in the failure.
func reconcileTenantKey(db *gorm.DB, table, probe string) error {
	var total, filled int64
	if err := db.Raw(probe).Row().Scan(&total, &filled); err != nil {
		return fmt.Errorf("reconcile %s: %w", table, err)
	}
	slog.Info("phase3 license tenant_key reconcile", "table", table, "total", total, "filled", filled)
	if filled != total {
		return fmt.Errorf("reconcile %s: backfill incomplete: %d of %d rows filled", table, filled, total)
	}
	return nil
}

// postMigrateDropLegacy closes the ④ window on the data side
// (deploy/phase4-drop-legacy.sql performs the identical procedure by hand):
// the retired app_secret credential column leaves license_apps. Guarded on
// the ③ tenant_key re-keying having converged (reconcileTenantKey above
// fails the run otherwise); DROP COLUMN IF EXISTS keeps it idempotent on
// fresh databases (testcontainers never carry the column).
func postMigrateDropLegacy(db *gorm.DB) error {
	//nolint:staticcheck // gorm.DB.Dialector is an interface field, not embedding
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	var hasCol bool
	if err := db.Raw(`SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'license_apps' AND column_name = 'app_secret')`).Scan(&hasCol).Error; err != nil {
		return err
	}
	if !hasCol {
		return nil // converged or fresh database
	}
	if err := db.Exec(`ALTER TABLE license_apps DROP COLUMN app_secret`).Error; err != nil {
		return fmt.Errorf("drop license_apps.app_secret: %w", err)
	}
	slog.Info("migrate: phase4 legacy-column drops complete")
	return nil
}
