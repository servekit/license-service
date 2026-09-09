// App-registry data access (license_apps). Mirrors the messaging/storage
// platform app pattern.
package dal

import (
	"context"

	"gorm.io/gorm"

	"github.com/servekit/license-service/internal/store/generated"
	"github.com/servekit/license-service/internal/store/models"
)

// CreateApp inserts a new calling-app row.
func CreateApp(ctx context.Context, tx *gorm.DB, a *models.LicenseApp) error {
	return gorm.G[models.LicenseApp](tx).Create(ctx, a)
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

// UpdateAppSecret replaces the app credential.
func UpdateAppSecret(ctx context.Context, tx *gorm.DB, id int64, secret string) error {
	_, err := gorm.G[models.LicenseApp](tx).
		Where(generated.LicenseApp.ID.Eq(id)).
		Set(generated.LicenseApp.AppSecret.Set(secret)).
		Update(ctx)
	return err
}

// DeleteApp hard-deletes the app row (licensing keeps no soft-delete rows).
func DeleteApp(ctx context.Context, tx *gorm.DB, id int64) error {
	_, err := gorm.G[models.LicenseApp](tx).
		Where(generated.LicenseApp.ID.Eq(id)).
		Delete(ctx)
	return err
}
