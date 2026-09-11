// Package tenantres tests: the identity resolution on the license data
// plane (gate semantics — the app row is purely a gate, license data itself
// is global). Since the ④ window close the trusted x-tenant-key is the only
// credential stack; the deleted legacy path is pinned Unauthenticated.
package tenantres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"google.golang.org/grpc/metadata"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/tenantctx"

	"github.com/servekit/license-service/internal/store/dal"
	"github.com/servekit/license-service/internal/store/models"
	"github.com/servekit/license-service/pkg/xcodes"
)

// legacyCtx plants the deleted stack's wire shape — anti-regression only.
func legacyCtx(ctx context.Context, appKey, appSecret string) context.Context {
	return metadata.NewIncomingContext(ctx, metadata.Pairs(
		"x-app-key", appKey,
		"x-app-secret", appSecret,
	))
}

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
	ctx := tenantctx.WithTenant(context.Background(), "ten_abc123def456")

	c1, err := r.Require(ctx)
	require.NoError(t, err)
	assert.Equal(t, "ten_abc123def456", c1.TenantKey)
	require.NotNil(t, c1.App)

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
	ctx := legacyCtx(context.Background(), "smuggled-app", "wrong-secret")
	ctx = tenantctx.WithTenant(ctx, "ten_platform")

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

	_, err := r.Require(tenantctx.WithTenant(context.Background(), tk))
	require.Error(t, err)
	require.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())
}

// TestRequire_TrustedReusesUnbackfilledAppKeyEqualRow: a pre-migration row
// whose app_key equals the tenant key but whose mapping column is still NULL
// is reused as-is — no duplicate row, no clobber of operator data.
func TestRequire_TrustedReusesUnbackfilledAppKeyEqualRow(t *testing.T) {
	r, db := newResolver(t)
	const key = "ten_unbackfill00" // unbackfill00 = 12
	seedApp(t, db, key, "lic_secret", nil, false)

	c, err := r.Require(tenantctx.WithTenant(context.Background(), key))
	require.NoError(t, err)
	require.NotNil(t, c.App)

	var n int64
	require.NoError(t, db.Model(&models.LicenseApp{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "no duplicate row created")
}

// TestRequire_LegacyCredentialsRejected (④ window close): the legacy ak/sk
// stack no longer authenticates — even previously-VALID pairs (mapped or
// unmapped) answer ErrAppUnauthorized, exactly like unknown apps, bad
// secrets, and disabled apps. Pinned against accidental resurrection.
func TestRequire_LegacyCredentialsRejected(t *testing.T) {
	r, db := newResolver(t)
	seedApp(t, db, "ak-mapped", "sk-1", models.TenantKeyPtr("ten_mapped000001"), false)
	seedApp(t, db, "ak-fallback", "sk-2", nil, false)
	seedApp(t, db, "ak-disabled", "sk-3", nil, true)

	_, err := r.Require(legacyCtx(context.Background(), "ak-mapped", "sk-1"))
	require.Error(t, err)
	require.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New(), "a previously-valid mapped pair must now be unauthenticated")

	_, err = r.Require(legacyCtx(context.Background(), "ak-fallback", "sk-2"))
	require.Error(t, err)
	require.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New(), "a previously-valid unmapped pair must now be unauthenticated")

	_, err = r.Require(legacyCtx(context.Background(), "ak-unknown", "sk-1"))
	require.Error(t, err)
	require.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())

	_, err = r.Require(legacyCtx(context.Background(), "ak-mapped", "sk-wrong"))
	require.Error(t, err)
	require.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())

	_, err = r.Require(legacyCtx(context.Background(), "ak-disabled", "sk-3"))
	require.Error(t, err)
	require.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())
}

// TestRequire_TrustedMalformedKeyRejected (④ window close): the trusted key
// is format-validated before any lookup or lazy create — legacy app_key
// literals and other malformed values answer ErrAppUnauthorized and never
// mint a gate row.
func TestRequire_TrustedMalformedKeyRejected(t *testing.T) {
	r, db := newResolver(t)

	for _, key := range []string{
		"ak-mapped",           // legacy app_key literal
		"testkit",             // legacy alias literal
		"ten_UPPERCASE00",     // uppercase
		"ten_short0",          // too short
		"ten_abc123def456789", // too long
		"ten_platform_system", // reserved literal with a suffix
	} {
		_, err := r.Require(tenantctx.WithTenant(context.Background(), key))
		assert.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New(), "malformed key %q must fail closed", key)
	}
	var n int64
	require.NoError(t, db.Model(&models.LicenseApp{}).Count(&n).Error)
	assert.Zero(t, n, "malformed keys must not lazily create gate rows")
}

// TestRequire_NoneFailsClosed: no usable credentials → the pre-③
// unauthenticated error.
func TestRequire_NoneFailsClosed(t *testing.T) {
	r, _ := newResolver(t)

	_, err := r.Require(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())

	// Half a legacy pair is still None.
	_, err = r.Require(legacyCtx(context.Background(), "", "sk"))
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
