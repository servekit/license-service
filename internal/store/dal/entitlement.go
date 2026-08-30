package dal

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/servekit/license-service/internal/store/generated"
	"github.com/servekit/license-service/internal/store/models"
)

// ListEntitlementsByKeyHash returns all module rows of a key (ascending by
// module for deterministic assembly).
func ListEntitlementsByKeyHash(ctx context.Context, tx *gorm.DB, keyHash string) ([]*models.LicenseEntitlement, error) {
	ents, err := gorm.G[models.LicenseEntitlement](tx).
		Where(generated.LicenseEntitlement.KeyHash.Eq(keyHash)).
		Order(generated.LicenseEntitlement.Module.Asc()).
		Find(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*models.LicenseEntitlement, len(ents))
	for i := range ents {
		out[i] = &ents[i]
	}
	return out, nil
}

// UpsertEntitlement inserts or replaces the (key_hash, module) row. The
// DO-UPDATE arm is a raw assignment map because expires_at must be clearable
// to NULL when a grant switches a module to perpetual (field.Time cannot
// SET NULL — documented exception); conditions stay on generated helpers.
func UpsertEntitlement(ctx context.Context, tx *gorm.DB, e *models.LicenseEntitlement) error {
	res := tx.Model(&models.LicenseEntitlement{}).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "key_hash"},
				{Name: "module"},
			},
			DoUpdates: clause.Assignments(map[string]any{
				"kind":       e.Kind,
				"expires_at": e.ExpiresAt,
				"granted_at": e.GrantedAt,
			}),
		}).
		Where(generated.LicenseEntitlement.KeyHash.Eq(e.KeyHash)).
		Where(generated.LicenseEntitlement.Module.Eq(e.Module)).
		Create(e)
	return res.Error
}

// DeleteEntitlement removes one module row (module becomes unlicensed on the
// next issuance).
func DeleteEntitlement(ctx context.Context, tx *gorm.DB, keyHash string, module int32) error {
	_, err := gorm.G[models.LicenseEntitlement](tx).
		Where(generated.LicenseEntitlement.KeyHash.Eq(keyHash)).
		Where(generated.LicenseEntitlement.Module.Eq(module)).
		Delete(ctx)
	return err
}

// DeleteEntitlementsByKeyHash removes every module row of a key (DeleteKey
// cascade leg).
func DeleteEntitlementsByKeyHash(ctx context.Context, tx *gorm.DB, keyHash string) error {
	_, err := gorm.G[models.LicenseEntitlement](tx).
		Where(generated.LicenseEntitlement.KeyHash.Eq(keyHash)).
		Delete(ctx)
	return err
}

// TouchEntitlementGranted refreshes granted_at (admin re-grant bookkeeping).
func TouchEntitlementGranted(ctx context.Context, tx *gorm.DB, keyHash string, module int32, at time.Time) error {
	_, err := gorm.G[models.LicenseEntitlement](tx).
		Where(generated.LicenseEntitlement.KeyHash.Eq(keyHash)).
		Where(generated.LicenseEntitlement.Module.Eq(module)).
		Set(generated.LicenseEntitlement.GrantedAt.Set(at)).
		Update(ctx)
	return err
}
