// Tenant-config admin RPCs for the licensing platform (phase ④ T6 rename
// of the app registry): one config row per tenant; the row keeps its
// internal calling-app identity — immutable, the secret minted server-side
// and echoed on every read (internal-trust posture; the ops console is the
// intended reader). Mutations log one admin_audit line like the rest of
// this package.
package admin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"

	licensev1 "github.com/servekit/api/gen/go/license/v1"
	"github.com/servekit/license-service/internal/store/dal"
	"github.com/servekit/license-service/internal/store/models"
	"github.com/servekit/license-service/pkg/xcodes"
)

// appKeyGenAttempts bounds the minted-app_key collision retry loop.
const appKeyGenAttempts = 3

// CreateTenantConfig registers a tenant's config row. The internal app
// identity is minted server-side ("lic_" + 8 base36 chars,
// collision-checked). The secret is echoed on every read of
// LicenseTenantConfigInfo. The row's tenant_key: a scoped caller is
// clamped to the injected key; the cross-view keeps its explicit
// tenant_key, else the minted app_key literal (the phase ③ window mapping
// the migration backfill also writes; T10 总装 remaps).
func (s *Service) CreateTenantConfig(ctx context.Context, req *licensev1.CreateTenantConfigRequest) (*licensev1.CreateTenantConfigResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	appKey, err := s.mintUniqueAppKey(ctx)
	if err != nil {
		return nil, err
	}

	secret, err := mintAppSecret()
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	app := &models.LicenseApp{
		AppKey:    appKey,
		AppSecret: secret,
		Name:      req.GetName(),
		TenantKey: models.TenantKeyPtr(clampTenantKey(scope, req.GetTenantKey(), appKey)),
	}
	if err := dal.CreateApp(ctx, s.db, app); err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	audit("create_tenant_config", "app:"+app.AppKey, "")
	return &licensev1.CreateTenantConfigResponse{Config: appToProto(app), AppSecret: secret}, nil
}

// GetTenantConfig returns the tenant's config row (tenant_key selector;
// pre-backfill rows resolve through the app_key-literal fallback).
func (s *Service) GetTenantConfig(ctx context.Context, req *licensev1.GetTenantConfigRequest) (*licensev1.GetTenantConfigResponse, error) {
	app, err := s.configForTenantScoped(ctx, req.GetTenantKey())
	if err != nil {
		return nil, err
	}
	return &licensev1.GetTenantConfigResponse{Config: appToProto(app)}, nil
}

// UpdateTenantConfig edits mutable fields; the row's identity is
// immutable. Absent optional fields keep their current values.
func (s *Service) UpdateTenantConfig(ctx context.Context, req *licensev1.UpdateTenantConfigRequest) (*licensev1.UpdateTenantConfigResponse, error) {
	app, err := s.configForTenantScoped(ctx, req.GetTenantKey())
	if err != nil {
		return nil, err
	}
	if req.Name != nil {
		app.Name = *req.Name
	}
	if req.Disabled != nil {
		app.Disabled = *req.Disabled
	}
	if err := dal.UpdateApp(ctx, s.db, app); err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	audit("update_tenant_config", fmt.Sprintf("app:%s disabled=%v", app.AppKey, app.Disabled), "")
	return &licensev1.UpdateTenantConfigResponse{Config: appToProto(app)}, nil
}

// RotateTenantConfigSecret mints a new secret; the old one stops working
// immediately (verification reads the DB on every call).
func (s *Service) RotateTenantConfigSecret(ctx context.Context, req *licensev1.RotateTenantConfigSecretRequest) (*licensev1.RotateTenantConfigSecretResponse, error) {
	app, err := s.configForTenantScoped(ctx, req.GetTenantKey())
	if err != nil {
		return nil, err
	}
	secret, err := mintAppSecret()
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	if err := dal.UpdateAppSecret(ctx, s.db, app.ID, secret); err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	app.AppSecret = secret
	audit("rotate_tenant_config_secret", "app:"+app.AppKey, "")
	return &licensev1.RotateTenantConfigSecretResponse{Config: appToProto(app), AppSecret: secret}, nil
}

// ListTenantConfigs lists the config rows in the caller's scope: for an
// injected key only the tenant's row; the cross-view the whole registry
// (one row per tenant, low cardinality, no paging).
func (s *Service) ListTenantConfigs(ctx context.Context, _ *licensev1.ListTenantConfigsRequest) (*licensev1.ListTenantConfigsResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	apps, err := dal.ListApps(ctx, s.db)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	out := make([]*licensev1.LicenseTenantConfigInfo, 0, len(apps))
	for _, a := range apps {
		if !visibleInTenant(scope, a) {
			continue
		}
		out = append(out, appToProto(a))
	}
	return &licensev1.ListTenantConfigsResponse{Configs: out}, nil
}

// DeleteTenantConfig removes the tenant's config row (hard delete —
// licensing keeps no soft-delete rows). Existing licenses and devices are
// untouched; the row's identity becomes reusable.
func (s *Service) DeleteTenantConfig(ctx context.Context, req *licensev1.DeleteTenantConfigRequest) (*emptypb.Empty, error) {
	app, err := s.configForTenantScoped(ctx, req.GetTenantKey())
	if err != nil {
		return nil, err
	}
	if err := dal.DeleteApp(ctx, s.db, app.ID); err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	audit("delete_tenant_config", "app:"+app.AppKey, "")
	return &emptypb.Empty{}, nil
}

// --- helpers ---

// configForTenantScoped is the resolver every tenant-config RPC funnels
// through (the T5 appByKeyScoped shape, re-keyed to the tenant selector in
// T6): fail closed on a caller with no trusted identity, resolve the row
// by tenant_key (pre-backfill NULL-column rows fall back to the app_key
// literal), then enforce the scope AFTER the row load — a foreign tenant's
// row answers the same not-found a missing key would (anti-enumeration).
func (s *Service) configForTenantScoped(ctx context.Context, tenantKey string) (*models.LicenseApp, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	a, err := dal.GetAppForTenant(ctx, s.db, tenantKey)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	if a == nil {
		return nil, xcodes.ErrAppNotFound.New()
	}
	if err := authorizeAppTenant(scope, a, xcodes.ErrAppNotFound.New()); err != nil {
		return nil, err
	}
	return a, nil
}

func appToProto(a *models.LicenseApp) *licensev1.LicenseTenantConfigInfo {
	return &licensev1.LicenseTenantConfigInfo{
		Id:        a.ID,
		AppKey:    a.AppKey,
		AppSecret: a.AppSecret,
		Name:      a.Name,
		Disabled:  a.Disabled,
		TenantKey: models.TenantKeyOf(a.TenantKey),
		CreatedAt: timestamppb.New(a.CreatedAt),
		UpdatedAt: timestamppb.New(a.UpdatedAt),
	}
}

// mintUniqueAppKey mints app_keys with collision retry: up to 3 attempts
// against the live table.
func (s *Service) mintUniqueAppKey(ctx context.Context) (string, error) {
	for range appKeyGenAttempts {
		candidate := mintAppKey()
		_, err := dal.GetAppByKey(ctx, s.db, candidate)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return candidate, nil
		}
		if err != nil {
			return "", xcodes.ErrInternal.Wrap(err)
		}
	}
	return "", xcodes.ErrInternal.New("mint app key: too many collisions")
}

// mintAppKey mints "lic_" + 8 base36 chars (same shape as the other
// platforms' "app_"/"sto_" keys).
func mintAppKey() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "lic_" + fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"
	out := make([]byte, 8)
	for i, b := range buf {
		out[i] = base36[int(b)%36]
	}
	return "lic_" + string(out)
}

// mintAppSecret mints "lic_" + 32 random bytes (base64url).
func mintAppSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint app secret: %w", err)
	}
	return "lic_" + base64.RawURLEncoding.EncodeToString(buf), nil
}
