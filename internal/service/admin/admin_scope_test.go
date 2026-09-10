// Admin actor-scope matrix (tenant platform phase ④ T5): the three caller
// states the LicenseAdminService surface must branch on —
//
//	injected key   → pinned: the app platform answers only the row mapped
//	                 to that tenant (creates derive tenant_key from the
//	                 injection); the key lifecycle (keys / entitlements /
//	                 devices / trials / pubkey — platform-global assets
//	                 with no tenant dimension) refuses
//	PLATFORM actor → cross-view full surface
//	no identity    → fail closed
//
// Foreign apps answer the domain not-found error (anti-enumeration).
package admin_test

import (
	"context"
	"testing"

	commonv1 "github.com/servekit/api/gen/go/common/v1"
	licensev1 "github.com/servekit/api/gen/go/license/v1"
	userv1 "github.com/servekit/api/gen/go/user/v1"
	"github.com/servekit/go-common/grpcx"
	"github.com/servekit/go-common/tenantctx"
	"github.com/servekit/license-service/internal/store/dal"
	"github.com/servekit/license-service/internal/store/models"
	"github.com/servekit/license-service/pkg/xcodes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	scopeAlpha = "ten_alpha0000000"
	scopeBeta  = "ten_beta0000000"
)

func platformCtx() context.Context {
	return grpcx.WithActor(context.Background(), &commonv1.RequestActor{
		UserId:   7,
		UserType: int32(userv1.UserType_USER_TYPE_PLATFORM),
	})
}

func tenantCtx(key string) context.Context {
	return tenantctx.WithTenantKey(context.Background(), key)
}

func anonCtx() context.Context { return context.Background() }

// seedScopedApps plants one app row per tenant.
func seedScopedApps(t *testing.T, h *harness) {
	t.Helper()
	for _, tc := range []struct{ appKey, tenant string }{
		{"alpha-app", scopeAlpha},
		{"beta-app", scopeBeta},
	} {
		app := &models.LicenseApp{
			AppKey: tc.appKey, AppSecret: "s", Name: tc.appKey,
			TenantKey: models.TenantKeyPtr(tc.tenant),
		}
		require.NoError(t, dal.CreateApp(context.Background(), h.db, app))
	}
}

// TestAdminScope_NoIdentityFailsClosed: neither an injected key nor a
// verified actor → every admin surface refuses.
func TestAdminScope_NoIdentityFailsClosed(t *testing.T) {
	h := newHarness(t)
	ctx := anonCtx()

	_, err := h.admin.ListTenantConfigs(ctx, &licensev1.ListTenantConfigsRequest{})
	require.Error(t, err)
	assert.ErrorIs(t, err, xcodes.ErrUnauthorized.New())

	_, err = h.admin.CreateKey(ctx, &licensev1.CreateKeyRequest{})
	require.ErrorIs(t, err, xcodes.ErrUnauthorized.New())

	_, err = h.admin.ListKeys(ctx, &licensev1.ListKeysRequest{})
	require.ErrorIs(t, err, xcodes.ErrUnauthorized.New())

	_, err = h.admin.CreateTenantConfig(ctx, &licensev1.CreateTenantConfigRequest{Name: "x"})
	require.ErrorIs(t, err, xcodes.ErrUnauthorized.New())
}

// TestAdminScope_AppPlatformBranch: the app platform under an injected key —
// lists answer the own row only; foreign rows answer not-found; creates
// derive tenant_key from the injection; the cross-view sees and does all.
func TestAdminScope_AppPlatformBranch(t *testing.T) {
	h := newHarness(t)
	seedScopedApps(t, h)
	ctx := tenantCtx(scopeAlpha)

	list, err := h.admin.ListTenantConfigs(ctx, &licensev1.ListTenantConfigsRequest{})
	require.NoError(t, err)
	require.Len(t, list.GetConfigs(), 1)
	assert.Equal(t, "alpha-app", list.GetConfigs()[0].GetAppKey())

	_, err = h.admin.GetTenantConfig(ctx, &licensev1.GetTenantConfigRequest{TenantKey: scopeBeta})
	require.ErrorIs(t, err, xcodes.ErrAppNotFound.New())

	_, err = h.admin.UpdateTenantConfig(ctx, &licensev1.UpdateTenantConfigRequest{TenantKey: scopeBeta})
	require.ErrorIs(t, err, xcodes.ErrAppNotFound.New())

	_, err = h.admin.RotateTenantConfigSecret(ctx, &licensev1.RotateTenantConfigSecretRequest{TenantKey: scopeBeta})
	require.ErrorIs(t, err, xcodes.ErrAppNotFound.New())

	_, err = h.admin.DeleteTenantConfig(ctx, &licensev1.DeleteTenantConfigRequest{TenantKey: scopeBeta})
	require.ErrorIs(t, err, xcodes.ErrAppNotFound.New())

	_, err = h.admin.GetTenantConfig(ctx, &licensev1.GetTenantConfigRequest{TenantKey: scopeAlpha})
	require.NoError(t, err)

	// scoped create derives tenant_key from the injection (the literal
	// window mapping is overridden); a fresh tenant keeps its one-row
	// budget. The app identity is minted server-side — read via the tenant.
	_, err = h.admin.CreateTenantConfig(tenantCtx("ten_gamma0000000"), &licensev1.CreateTenantConfigRequest{Name: "gamma"})
	require.NoError(t, err)
	row, err := dal.GetAppForTenant(context.Background(), h.db, "ten_gamma0000000")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "ten_gamma0000000", models.TenantKeyOf(row.TenantKey))

	// cross-view keeps the literal fallback (the minted app key stamps
	// itself) and manages everything
	cross, err := h.admin.CreateTenantConfig(platformCtx(), &licensev1.CreateTenantConfigRequest{Name: "ops"})
	require.NoError(t, err)
	opsKey := cross.GetConfig().GetAppKey()
	opsRow, err := dal.GetAppByKey(context.Background(), h.db, opsKey)
	require.NoError(t, err)
	assert.Equal(t, opsKey, models.TenantKeyOf(opsRow.TenantKey), "cross-view keeps the minted literal stamp")
	all, err := h.admin.ListTenantConfigs(platformCtx(), &licensev1.ListTenantConfigsRequest{})
	require.NoError(t, err)
	assert.Len(t, all.GetConfigs(), 4) // alpha, beta, gamma, ops
	_, err = h.admin.RotateTenantConfigSecret(platformCtx(), &licensev1.RotateTenantConfigSecretRequest{TenantKey: scopeBeta})
	require.NoError(t, err)
}

// TestAdminScope_KeyLifecyclePlatformOnly: license keys are platform-global
// assets (no tenant dimension) — the key lifecycle admits only the PLATFORM
// cross-view; a tenant-scoped caller is refused.
func TestAdminScope_KeyLifecyclePlatformOnly(t *testing.T) {
	h := newHarness(t)
	ctx := tenantCtx(scopeAlpha)

	_, err := h.admin.CreateKey(ctx, &licensev1.CreateKeyRequest{})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = h.admin.ListKeys(ctx, &licensev1.ListKeysRequest{})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = h.admin.ShowKey(ctx, &licensev1.ShowKeyRequest{KeyId: "lk_x"})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = h.admin.ShowTrial(ctx, &licensev1.ShowTrialRequest{FingerprintId: fp1})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = h.admin.ShowPubKey(ctx, &licensev1.ShowPubKeyRequest{})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	// cross-view works end-to-end
	created, err := h.admin.CreateKey(platformCtx(), &licensev1.CreateKeyRequest{})
	require.NoError(t, err)
	require.NotEmpty(t, created.GetPlaintextKey())
}
