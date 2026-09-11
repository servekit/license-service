// Package tenantres resolves the license data plane's calling identity.
// Since the ④ window close the trusted x-tenant-key (injected by the portal
// proxy) is the ONLY credential stack:
//
//   - the key is format-validated first (tenantctx.ValidTenantKey — the
//     canonical ten_[0-9a-z]{12} shape plus the reserved ten_platform /
//     ten_legacy literals); malformed keys fail closed before any lookup
//     or lazy create;
//   - the gate row (license_apps row — the ONLY per-tenant artifact;
//     license keys/devices/trials/entitlements are global) is resolved by
//     the tenant_key mapping and lazily upserted on first sight. The gate
//     is the whole effect: nothing downstream keys on the tenant (license
//     data is global per spec).
//
// Unlike message/storage this resolver keeps the pre-③ read posture — the
// DB on every call, no registry snapshot (activation traffic is not hot
// enough to warrant one).
package tenantres

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/servekit/go-common/tenantctx"

	"github.com/servekit/license-service/internal/store/dal"
	"github.com/servekit/license-service/internal/store/models"
	"github.com/servekit/license-service/pkg/xcodes"
)

// Caller is the resolved data-plane identity: the tenant context plus the
// gate row. In license-service the row is purely a gate (disabled = closed
// door); TenantKey is carried for shape parity with message/storage — no
// license-side keying hangs off it today.
type Caller struct {
	// TenantKey is the authoritative tenant context ("ten_..." or one of
	// the reserved literals).
	TenantKey string
	// App is the tenant's (possibly just-created) gate row.
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

// Require resolves the Caller from the trusted x-tenant-key, failing closed
// with ErrAppUnauthorized when the credential is missing or malformed, or
// the resolved gate row is unknown or disabled. Call at the entry of every
// credential-presenting surface (Activate / Deactivate / TrialStart).
func (r *Resolver) Require(ctx context.Context) (*Caller, error) {
	tenantKey, ok := tenantctx.TrustedKeyFromIncoming(ctx)
	if !ok {
		return nil, xcodes.ErrAppUnauthorized.New(
			"missing trusted caller credential (x-tenant-key metadata)")
	}
	if !tenantctx.ValidTenantKey(tenantKey) {
		return nil, xcodes.ErrAppUnauthorized.New(fmt.Sprintf("malformed x-tenant-key %q", tenantKey))
	}
	return r.ensureTrusted(ctx, tenantKey)
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
		if err := dal.EnsureTenantApp(ctx, r.db, &models.LicenseApp{
			AppKey:    tenantKey,
			TenantKey: models.TenantKeyPtr(tenantKey),
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

