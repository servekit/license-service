// Package tenantres resolves the license data plane's calling identity
// during the phase ③ dual-stack window (recipe step 1, rule D-③1):
//
//   - SourceTrusted (x-tenant-key, injected by the portal proxy): the tenant
//     key IS the tenant context. The gate row (license_apps row — the ONLY
//     per-tenant artifact; license keys/devices/trials/entitlements are
//     global) is resolved by the tenant_key mapping and lazily upserted on
//     first sight. The gate is the whole effect: nothing downstream keys on
//     the tenant (license data is global per spec).
//   - SourceLegacy (x-app-key/x-app-secret): the pre-③ app validation,
//     unchanged; the validated app is then converted to the tenant its row
//     maps to (tenant_key column; empty column falls back to the app_key
//     literal — T10 总装 clears the empties).
//   - SourceNone: unauthenticated, fail closed.
//
// Unlike message/storage this resolver keeps the pre-③ read posture — the
// DB on every call, no registry snapshot (activation traffic is not hot
// enough to warrant one).
//
// The whole package (and the legacy half of appauth) is deleted when the
// window closes (phase ④).
package tenantres

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/servekit/go-common/dualauth"

	"github.com/servekit/license-service/internal/appauth"
	"github.com/servekit/license-service/internal/store/dal"
	"github.com/servekit/license-service/internal/store/models"
	"github.com/servekit/license-service/pkg/xcodes"
)

// Caller is the resolved data-plane identity: the tenant context plus the
// gate row. In license-service the row is purely a gate (disabled = closed
// door); TenantKey is carried for shape parity with message/storage and for
// the T10 switch-over — no license-side keying hangs off it today.
type Caller struct {
	// TenantKey is the authoritative tenant context ("ten_..." or a legacy
	// app_key literal through the window).
	TenantKey string
	// App is the gate row: the validated app on the legacy path, the
	// tenant's (possibly just-created) row on the trusted path.
	App *models.LicenseApp
}

// Resolver resolves callers; every call reads the DB (pre-③ posture).
type Resolver struct {
	db *gorm.DB
}

// New constructs a Resolver.
func New(db *gorm.DB) *Resolver {
	return &Resolver{db: db}
}

// Require classifies the caller's credential stack (appauth.Resolve, D-③1)
// and resolves the Caller, failing closed with ErrAppUnauthorized on missing
// credentials, unknown/disabled apps, or a bad secret. Call at the entry of
// every credential-presenting surface (Activate / Deactivate / TrialStart).
func (r *Resolver) Require(ctx context.Context) (*Caller, error) {
	tenantKey, appKey, appSecret, source := appauth.Resolve(ctx)
	switch source {
	case dualauth.SourceTrusted:
		return r.ensureTrusted(ctx, tenantKey)
	case dualauth.SourceLegacy:
		return r.verifyLegacy(ctx, appKey, appSecret)
	default:
		return nil, xcodes.ErrAppUnauthorized.New(
			"missing caller credentials (x-tenant-key or x-app-key / x-app-secret metadata)")
	}
}

// ensureTrusted resolves the tenant's gate row; on a miss it falls back to
// the app_key-equal shape (pre-backfill window rows) and finally lazily
// creates the default gate row. The minted secret satisfies the not-null
// column without being handed to anyone — trusted callers authenticate by
// network position, never by secret. A disabled gate row fails closed.
func (r *Resolver) ensureTrusted(ctx context.Context, tenantKey string) (*Caller, error) {
	app, err := dal.GetAppForTenant(ctx, r.db, tenantKey)
	if err != nil {
		return nil, err
	}
	if app == nil {
		secret, mintErr := mintTenantSecret()
		if mintErr != nil {
			return nil, xcodes.ErrInternal.Wrap(mintErr)
		}
		if err := dal.EnsureTenantApp(ctx, r.db, &models.LicenseApp{
			AppKey:    tenantKey,
			TenantKey: models.TenantKeyPtr(tenantKey),
			AppSecret: secret,
			Name:      tenantKey,
		}); err != nil {
			return nil, err
		}
		app, err = dal.GetAppForTenant(ctx, r.db, tenantKey)
		if err != nil {
			return nil, err
		}
		if app == nil {
			return nil, xcodes.ErrInternal.New("ensure tenant gate row: row absent after insert")
		}
	}
	if app.Disabled {
		return nil, xcodes.ErrAppUnauthorized.New(fmt.Sprintf("unknown or disabled app %q", tenantKey))
	}
	return &Caller{TenantKey: tenantKey, App: app}, nil
}

// verifyLegacy is the pre-③ appauth path, kept verbatim through the
// dual-stack window: unknown/disabled app or a bad secret fails closed. The
// validated app is then converted to its mapped tenant_key (empty column →
// app_key literal fallback; T10 总装 clears the empties).
func (r *Resolver) verifyLegacy(ctx context.Context, appKey, appSecret string) (*Caller, error) {
	app, err := dal.GetAppByKey(ctx, r.db, appKey)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, xcodes.ErrAppUnauthorized.New("unknown app credentials")
	}
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	if app.Disabled {
		return nil, xcodes.ErrAppUnauthorized.New("app is disabled")
	}
	if app.AppSecret != appSecret {
		return nil, xcodes.ErrAppUnauthorized.New("invalid app secret")
	}
	return &Caller{TenantKey: models.AppTenantKey(app), App: app}, nil
}

// mintTenantSecret mints "lic_" + 32 random bytes (base64url) — same shape
// as admin-minted app secrets; never handed to anyone (trusted callers
// authenticate by network position).
func mintTenantSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint tenant secret: %w", err)
	}
	return "lic_" + base64.RawURLEncoding.EncodeToString(buf), nil
}
