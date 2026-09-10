// Package tenantres tests: the phase ③ dual-stack identity resolution on the
// license data plane (gate semantics — the app row is purely a gate, license
// data itself is global).
package tenantres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/servekit/go-common/dbx"

	"github.com/servekit/license-service/internal/appauth"
	"github.com/servekit/license-service/internal/store/dal"
	"github.com/servekit/license-service/internal/store/models"
	"github.com/servekit/license-service/pkg/xcodes"
)

func newResolver(t *testing.T) (*Resolver, *gorm.DB) {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, dbx.AutoMigrate(db, models.AllModels()...))
	return New(db), db
}

func seedApp(t *testing.T, db *gorm.DB, appKey, secret string, tenantKey *string, disabled bool) {
	t.Helper()
	require.NoError(t, db.Create(&models.LicenseApp{
		AppKey:    appKey,
		AppSecret: secret,
		Name:      appKey,
		TenantKey: tenantKey,
		Disabled:  disabled,
	}).Error)
}

// TestRequire_TrustedFirstSightLazilyCreatesGateRow: a trusted tenant key
// never seen before gets its gate row created on the spot (app_key =
// tenant_key, mapping column set, random secret never handed to anyone) and
// the second call reuses it — one row per tenant.
func TestRequire_TrustedFirstSightLazilyCreatesGateRow(t *testing.T) {
	r, db := newResolver(t)
	ctx := appauth.WithTenant(context.Background(), "ten_abc123def456")

	c1, err := r.Require(ctx)
	require.NoError(t, err)
	assert.Equal(t, "ten_abc123def456", c1.TenantKey)
	require.NotNil(t, c1.App)
	assert.NotEmpty(t, c1.App.AppSecret, "minted secret satisfies the not-null column")

	var n int64
	require.NoError(t, db.Model(&models.LicenseApp{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "first sight creates exactly one gate row")

	got, err := dal.GetAppByKey(context.Background(), db, "ten_abc123def456")
	require.NoError(t, err)
	assert.Equal(t, "ten_abc123def456", models.TenantKeyOf(got.TenantKey), "lazy row carries the mapping")
	assert.Equal(t, c1.App.ID, got.ID)

	// Second sight reuses the row (idempotent upsert converges).
	c2, err := r.Require(ctx)
	require.NoError(t, err)
	assert.Equal(t, c1.App.ID, c2.App.ID, "second sight reuses the same row")
	require.NoError(t, db.Model(&models.LicenseApp{}).Count(&n).Error)
	assert.Equal(t, int64(1), n)
}

// TestRequire_TrustedDiscardsSmuggledLegacyCredentials: D-③1 — when
// x-tenant-key rides along legacy credentials, the tenant key is
// authoritative and the legacy pair is ignored even when bogus.
func TestRequire_TrustedDiscardsSmuggledLegacyCredentials(t *testing.T) {
	r, _ := newResolver(t)
	ctx := appauth.WithApp(context.Background(), "smuggled-app", "wrong-secret")
	ctx = appauth.WithTenant(ctx, "ten_platform")

	c, err := r.Require(ctx)
	require.NoError(t, err)
	assert.Equal(t, "ten_platform", c.TenantKey)
}

// TestRequire_TrustedDisabledGateRowFailsClosed: a disabled gate row blocks
// the tenant's whole data plane.
func TestRequire_TrustedDisabledGateRowFailsClosed(t *testing.T) {
	r, db := newResolver(t)
	tk := "ten_disabled0001"
	seedApp(t, db, tk, "s", models.TenantKeyPtr(tk), true)

	_, err := r.Require(appauth.WithTenant(context.Background(), tk))
	require.Error(t, err)
	require.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())
}

// TestRequire_TrustedReusesUnbackfilledAppKeyEqualRow: a pre-migration row
// whose app_key equals the tenant key but whose mapping column is still NULL
// is reused as-is — no duplicate row, no clobber of operator data.
func TestRequire_TrustedReusesUnbackfilledAppKeyEqualRow(t *testing.T) {
	r, db := newResolver(t)
	seedApp(t, db, "testkit", "lic_secret", nil, false)

	c, err := r.Require(appauth.WithTenant(context.Background(), "testkit"))
	require.NoError(t, err)
	require.NotNil(t, c.App)
	assert.Equal(t, "lic_secret", c.App.AppSecret, "existing row reused verbatim")

	var n int64
	require.NoError(t, db.Model(&models.LicenseApp{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "no duplicate row created")
}

// TestRequire_LegacyPathUnchanged: the pre-③ validation semantics hold —
// unknown app, bad secret, and disabled app all fail closed; a valid pair
// resolves to the app converted to its mapped tenant_key.
func TestRequire_LegacyPathUnchanged(t *testing.T) {
	r, db := newResolver(t)
	seedApp(t, db, "ak-mapped", "sk-1", models.TenantKeyPtr("ten_mapped00001"), false)
	seedApp(t, db, "ak-fallback", "sk-2", nil, false)
	seedApp(t, db, "ak-disabled", "sk-3", nil, true)

	c, err := r.Require(appauth.WithApp(context.Background(), "ak-mapped", "sk-1"))
	require.NoError(t, err)
	assert.Equal(t, "ten_mapped00001", c.TenantKey, "mapped column wins on the legacy path")

	c, err = r.Require(appauth.WithApp(context.Background(), "ak-fallback", "sk-2"))
	require.NoError(t, err)
	assert.Equal(t, "ak-fallback", c.TenantKey, "empty column falls back to the app_key literal (T10 clears)")

	_, err = r.Require(appauth.WithApp(context.Background(), "ak-unknown", "sk-1"))
	require.Error(t, err)
	require.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())

	_, err = r.Require(appauth.WithApp(context.Background(), "ak-mapped", "sk-wrong"))
	require.Error(t, err)
	require.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())

	_, err = r.Require(appauth.WithApp(context.Background(), "ak-disabled", "sk-3"))
	require.Error(t, err)
	require.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())
}

// TestRequire_NoneFailsClosed: no usable credentials → the pre-③
// unauthenticated error.
func TestRequire_NoneFailsClosed(t *testing.T) {
	r, _ := newResolver(t)

	_, err := r.Require(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())

	// Half a legacy pair is still None.
	_, err = r.Require(appauth.WithApp(context.Background(), "", "sk"))
	require.Error(t, err)
	require.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())
}

// TestAppTenantKeyNilSafe pins the helper's nil tolerance (defensive shape
// shared with message/storage).
func TestAppTenantKeyNilSafe(t *testing.T) {
	assert.Equal(t, "", models.AppTenantKey(nil))
	assert.Equal(t, "", models.TenantKeyOf(nil))
	assert.Nil(t, models.TenantKeyPtr(""))
	assert.Equal(t, "ten_x", *models.TenantKeyPtr("ten_x"))
}
