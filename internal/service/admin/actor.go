// Actor-scope helpers for the license admin surface (tenant platform
// phase ④ T5 — server-side closure), mirroring portal-service's
// internal/service/admin/actor.go shape.
//
// The two trusted identity inputs are the ones the doors plant:
//
//   - x-tenant-key (tenantctx): the management-plane tenant choice the
//     testkit gate resolved and injected. Present → the caller is pinned to
//     that tenant (TENANT_ADMIN self-service and PLATFORM drill-down alike).
//   - x-actor (grpcx): the verified console identity. A PLATFORM actor with
//     NO injection is the cross-view ("drill everywhere") posture.
//
// License's admin surface splits in two under this scope:
//
//   - the app platform (CreateApp / GetApp / UpdateApp / RotateAppSecret /
//     ListApps / DeleteApp) carries the phase ③ tenant mapping and is
//     tenant-scopable exactly like the message/telemetry/storage app
//     surfaces;
//   - the key lifecycle (keys / entitlements / devices / trials / pubkey)
//     has NO tenant dimension — license keys are platform-global assets,
//     so those RPCs admit only the PLATFORM cross-view
//     (requirePlatformScope), matching the console's canPlatform routes.
//
// Anything else — a TENANT_ADMIN whose choice never resolved, an END_USER,
// a caller with neither identity — fails closed: the internal-network-only
// window closes here. App-row denials answer in the domain not-found style
// (anti-enumeration, aligned with the ErrUserNotFound ruling).
package admin

import (
	"context"

	userv1 "github.com/servekit/api/gen/go/user/v1"
	"github.com/servekit/go-common/grpcx"
	"github.com/servekit/go-common/tenantctx"

	"github.com/servekit/license-service/internal/store/models"
	"github.com/servekit/license-service/pkg/xcodes"
)

// scopeFromCtx resolves the caller's admin scope:
//
//	("", nil)  → PLATFORM cross-view (full surface, no injection)
//	(key, nil) → pinned to the injected tenant key
//	("", err)  → fail closed (no trusted identity at all)
//
// The injected key outranks the actor: a PLATFORM drill-down is scoped
// exactly like the tenant it is inspecting.
func scopeFromCtx(ctx context.Context) (string, error) {
	if key, ok := tenantctx.TenantKeyFromCtx(ctx); ok {
		return key, nil
	}
	actor, err := grpcx.MustActorFromCtx(ctx)
	if err != nil {
		return "", xcodes.ErrUnauthorized.New()
	}
	if userv1.UserType(actor.GetUserType()) != userv1.UserType_USER_TYPE_PLATFORM {
		return "", xcodes.ErrForbidden.New("management plane requires a platform actor or a tenant injection")
	}
	return "", nil
}

// requirePlatformScope admits only the PLATFORM cross-view (no injected
// key). For surfaces with no tenant dimension this is the whole closure:
// a tenant-scoped caller is refused outright, and no identity fails closed.
func requirePlatformScope(ctx context.Context) error {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return err
	}
	if scope != "" {
		return xcodes.ErrForbidden.New("platform operators only")
	}
	return nil
}

// visibleInTenant reports whether an app row is LISTABLE by the scope. The
// cross-view ("") sees everything; a scoped caller sees only rows whose
// resolved tenant (the tenant_key column, else the app_key literal — the
// ③ backfill fallback) equals their scope.
func visibleInTenant(scope string, app *models.LicenseApp) bool {
	if scope == "" {
		return true
	}
	return models.AppTenantKey(app) == scope
}

// authorizeAppTenant is the per-app rule after the row load: the cross-view
// touches everything; a scoped caller only the app mapped to their tenant —
// anything else answers with the caller-supplied not-found error so another
// tenant's app_key existence never leaks (anti-enumeration).
func authorizeAppTenant(scope string, app *models.LicenseApp, notFound error) error {
	if scope == "" {
		return nil
	}
	if models.AppTenantKey(app) == scope {
		return nil
	}
	return notFound
}

// clampTenantKey applies the create-side rule: a scoped caller's app is
// stamped with the injected key (license creates take no tenant_key from
// the wire — the app_key-literal fallback is overridden for scoped
// callers). The cross-view keeps the literal fallback.
func clampTenantKey(scope, appKey string) string {
	if scope == "" {
		return appKey
	}
	return scope
}
