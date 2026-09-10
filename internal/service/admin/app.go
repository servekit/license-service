// App-registry admin RPCs: calling-application CRUD for the licensing
// platform. Mirrors message-service's app management — app_key is immutable,
// the secret is minted server-side and echoed on every read (internal-trust
// posture; the ops console is the intended reader). Mutations log one
// admin_audit line like the rest of this package.
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

// CreateApp registers a calling application. app_key empty = minted
// server-side ("lic_" + 8 base36 chars, collision-checked). The secret is
// echoed on every read of LicenseAppInfo.
func (s *Service) CreateApp(ctx context.Context, req *licensev1.CreateAppRequest) (*licensev1.CreateAppResponse, error) {
	appKey := req.GetAppKey()
	if appKey == "" {
		var err error
		appKey, err = s.mintUniqueAppKey(ctx)
		if err != nil {
			return nil, err
		}
	} else if _, err := dal.GetAppByKey(ctx, s.db, appKey); err == nil {
		return nil, xcodes.ErrAppExists.New(fmt.Sprintf("app_key %q already exists", appKey))
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	secret, err := mintAppSecret()
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	app := &models.LicenseApp{
		AppKey:    appKey,
		AppSecret: secret,
		Name:      req.GetName(),
		// Phase ③ window mapping: the migration backfill writes
		// tenant_key = app_key literal, so admin-created rows stamp the same
		// value (the trusted lazy upsert and legacy conversion both resolve
		// through it; T10 总装 remaps to ten_* keys).
		TenantKey: models.TenantKeyPtr(appKey),
	}
	if err := dal.CreateApp(ctx, s.db, app); err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	audit("create_app", "app:"+app.AppKey, "")
	return &licensev1.CreateAppResponse{App: appToProto(app), AppSecret: secret}, nil
}

// GetApp returns one app by app_key.
func (s *Service) GetApp(ctx context.Context, req *licensev1.GetAppRequest) (*licensev1.GetAppResponse, error) {
	app, err := s.appByKey(ctx, req.GetAppKey())
	if err != nil {
		return nil, err
	}
	return &licensev1.GetAppResponse{App: appToProto(app)}, nil
}

// UpdateApp edits mutable fields; app_key is immutable. Absent optional
// fields keep their current values.
func (s *Service) UpdateApp(ctx context.Context, req *licensev1.UpdateAppRequest) (*licensev1.UpdateAppResponse, error) {
	app, err := s.appByKey(ctx, req.GetAppKey())
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
	audit("update_app", fmt.Sprintf("app:%s disabled=%v", app.AppKey, app.Disabled), "")
	return &licensev1.UpdateAppResponse{App: appToProto(app)}, nil
}

// RotateAppSecret mints a new secret; the old one stops working immediately
// (verification reads the DB on every call).
func (s *Service) RotateAppSecret(ctx context.Context, req *licensev1.RotateAppSecretRequest) (*licensev1.RotateAppSecretResponse, error) {
	app, err := s.appByKey(ctx, req.GetAppKey())
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
	audit("rotate_app_secret", "app:"+app.AppKey, "")
	return &licensev1.RotateAppSecretResponse{App: appToProto(app), AppSecret: secret}, nil
}

// ListApps lists all apps (low cardinality, no paging).
func (s *Service) ListApps(ctx context.Context, _ *licensev1.ListAppsRequest) (*licensev1.ListAppsResponse, error) {
	apps, err := dal.ListApps(ctx, s.db)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	out := make([]*licensev1.LicenseAppInfo, 0, len(apps))
	for _, a := range apps {
		out = append(out, appToProto(a))
	}
	return &licensev1.ListAppsResponse{Apps: out}, nil
}

// DeleteApp removes the app row (hard delete — licensing keeps no
// soft-delete rows). Existing licenses and devices are untouched; the
// app_key becomes reusable.
func (s *Service) DeleteApp(ctx context.Context, req *licensev1.DeleteAppRequest) (*emptypb.Empty, error) {
	app, err := s.appByKey(ctx, req.GetAppKey())
	if err != nil {
		return nil, err
	}
	if err := dal.DeleteApp(ctx, s.db, app.ID); err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	audit("delete_app", "app:"+app.AppKey, "")
	return &emptypb.Empty{}, nil
}

// --- helpers ---

// appByKey resolves an app by app_key, mapping not-found to the domain error.
func (s *Service) appByKey(ctx context.Context, appKey string) (*models.LicenseApp, error) {
	a, err := dal.GetAppByKey(ctx, s.db, appKey)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, xcodes.ErrAppNotFound.New()
	}
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return a, nil
}

func appToProto(a *models.LicenseApp) *licensev1.LicenseAppInfo {
	return &licensev1.LicenseAppInfo{
		Id:        a.ID,
		AppKey:    a.AppKey,
		AppSecret: a.AppSecret,
		Name:      a.Name,
		Disabled:  a.Disabled,
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
