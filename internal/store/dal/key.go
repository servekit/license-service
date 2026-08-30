// Package dal provides type-safe data-access functions over generated GORM
// helpers. One file per table (mirroring models/); cross-table composition
// lives in the service layer. dal functions accept *gorm.DB (which may be a
// transaction passed down from the service layer) and NEVER open
// transactions themselves. Errors are returned raw — the service layer
// decides how to wrap them into xcodes.
package dal

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/servekit/license-service/internal/store/generated"
	"github.com/servekit/license-service/internal/store/models"
)

// CreateKey inserts a new key master row.
func CreateKey(ctx context.Context, tx *gorm.DB, k *models.LicenseKey) error {
	return gorm.G[models.LicenseKey](tx).Create(ctx, k)
}

// GetKeyByKeyHash fetches a key by its SHA-256 hash; gorm.ErrRecordNotFound
// when absent.
func GetKeyByKeyHash(ctx context.Context, tx *gorm.DB, keyHash string) (*models.LicenseKey, error) {
	k, err := gorm.G[models.LicenseKey](tx).
		Where(generated.LicenseKey.KeyHash.Eq(keyHash)).
		Take(ctx)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// GetKeyForUpdate fetches a key with SELECT ... FOR UPDATE so the slot
// judgment (count + insert) in the activation transaction serializes per
// key. Callers MUST be inside a transaction.
func GetKeyForUpdate(ctx context.Context, tx *gorm.DB, keyHash string) (*models.LicenseKey, error) {
	k, err := gorm.G[models.LicenseKey](tx,
		clause.Locking{Strength: "UPDATE"},
	).
		Where(generated.LicenseKey.KeyHash.Eq(keyHash)).
		Take(ctx)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// GetKeyByIDOrHash resolves the dual-form {key_id} used across the admin
// surface: "lk_"-prefixed license ids look up license_id, anything else is
// treated as a full 64-hex key_hash.
func GetKeyByIDOrHash(ctx context.Context, tx *gorm.DB, idOrHash string) (*models.LicenseKey, error) {
	var cond clause.Expression
	if strings.HasPrefix(idOrHash, "lk_") {
		cond = generated.LicenseKey.LicenseID.Eq(idOrHash)
	} else {
		cond = generated.LicenseKey.KeyHash.Eq(idOrHash)
	}
	k, err := gorm.G[models.LicenseKey](tx).
		Where(cond).
		Take(ctx)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// ListKeys returns up to limit keys, newest first; status 0 = all statuses.
func ListKeys(ctx context.Context, tx *gorm.DB, status int32, limit int) ([]*models.LicenseKey, error) {
	q := gorm.G[models.LicenseKey](tx).
		Order(generated.LicenseKey.CreatedAt.Desc()).
		Limit(limit)
	if status != 0 {
		q = q.Where(generated.LicenseKey.Status.Eq(status))
	}
	keys, err := q.Find(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*models.LicenseKey, len(keys))
	for i := range keys {
		out[i] = &keys[i]
	}
	return out, nil
}

// UpdateKeyLabel sets the operator label.
func UpdateKeyLabel(ctx context.Context, tx *gorm.DB, keyHash, label string) error {
	_, err := gorm.G[models.LicenseKey](tx).
		Where(generated.LicenseKey.KeyHash.Eq(keyHash)).
		Set(generated.LicenseKey.Label.Set(label)).
		Update(ctx)
	return err
}

// UpdateKeySlots sets the concurrent-slot bound.
func UpdateKeySlots(ctx context.Context, tx *gorm.DB, keyHash string, maxSlots int32) error {
	_, err := gorm.G[models.LicenseKey](tx).
		Where(generated.LicenseKey.KeyHash.Eq(keyHash)).
		Set(generated.LicenseKey.MaxSlots.Set(maxSlots)).
		Update(ctx)
	return err
}

// SetKeyStatus flips the soft revocation state. status carries the proto
// KeyStatus value already cast to int32 by the service layer; revokedAt is
// non-nil exactly while revoked (nil clears the column). Raw-typed
// Conditions stay on generated helpers; the map arm exists because the
// generic Update(ctx, name, value) is single-column and field.Time cannot
// SET NULL (documented exception).
func SetKeyStatus(ctx context.Context, tx *gorm.DB, keyHash string, status int32, revokedAt *time.Time) error {
	res := tx.Model(&models.LicenseKey{}).
		Where(generated.LicenseKey.KeyHash.Eq(keyHash)).
		Updates(map[string]any{
			"status":     status,
			"revoked_at": revokedAt,
		})
	return res.Error
}

// DeleteKey physically deletes the key row (admin DeleteKey; entitlement and
// device rows are removed in the same service-layer transaction).
func DeleteKey(ctx context.Context, tx *gorm.DB, keyHash string) error {
	_, err := gorm.G[models.LicenseKey](tx).
		Where(generated.LicenseKey.KeyHash.Eq(keyHash)).
		Delete(ctx)
	return err
}
