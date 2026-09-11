// App-registry data access (license_apps). Mirrors the messaging/storage
// platform app pattern.
package dal

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/servekit/license-service/internal/store/generated"
	"github.com/servekit/license-service/internal/store/models"
	"github.com/servekit/license-service/pkg/xcodes"
)

// CreateApp inserts a new calling-app row.
func CreateApp(ctx context.Context, tx *gorm.DB, a *models.LicenseApp) error {
	return gorm.G[models.LicenseApp](tx).Create(ctx, a)
}

// EnsureTenantApp idempotently inserts the first-sight tenant gate row
// (trusted x-tenant-key path). ON CONFLICT DO NOTHING (no target — the row
// carries both unique keys, app_key and tenant_key) + caller re-read, so
// racing replicas converge on one row and operator edits are never clobbered.
func EnsureTenantApp(ctx context.Context, tx *gorm.DB, record *models.LicenseApp) error {
	if err := gorm.G[models.LicenseApp](tx, clause.OnConflict{
		DoNothing: true,
	}).Create(ctx, record); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// GetAppForTenant resolves the tenant's gate row: prefer the tenant_key
// mapping, fall back to app_key = tenantKey (pre-backfill window rows whose
// column is still NULL). nil when neither matches.
func GetAppForTenant(ctx context.Context, tx *gorm.DB, tenantKey string) (*models.LicenseApp, error) {
	record, err := gorm.G[models.LicenseApp](tx).
		Where(generated.LicenseApp.TenantKey.Eq(tenantKey)).
		Take(ctx)
	if err == nil {
		return &record, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	record, err = gorm.G[models.LicenseApp](tx).
		Where(generated.LicenseApp.AppKey.Eq(tenantKey)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// GetAppByKey fetches an app by app_key; gorm.ErrRecordNotFound when absent.
func GetAppByKey(ctx context.Context, tx *gorm.DB, appKey string) (*models.LicenseApp, error) {
	a, err := gorm.G[models.LicenseApp](tx).
		Where(generated.LicenseApp.AppKey.Eq(appKey)).
		Take(ctx)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListApps returns all apps ordered by app_key (low cardinality, no paging).
func ListApps(ctx context.Context, tx *gorm.DB) ([]*models.LicenseApp, error) {
	apps, err := gorm.G[models.LicenseApp](tx).
		Order(generated.LicenseApp.AppKey).
		Find(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*models.LicenseApp, 0, len(apps))
	for i := range apps {
		out = append(out, &apps[i])
	}
	return out, nil
}

// UpdateApp persists mutable fields (name, disabled).
func UpdateApp(ctx context.Context, tx *gorm.DB, a *models.LicenseApp) error {
	res := tx.Model(&models.LicenseApp{}).
		Where(generated.LicenseApp.ID.Eq(a.ID)).
		Updates(map[string]any{
			"name":     a.Name,
			"disabled": a.Disabled,
		})
	return res.Error
}

// DeleteApp hard-deletes the app row (licensing keeps no soft-delete rows).
func DeleteApp(ctx context.Context, tx *gorm.DB, id int64) error {
	_, err := gorm.G[models.LicenseApp](tx).
		Where(generated.LicenseApp.ID.Eq(id)).
		Delete(ctx)
	return err
}
