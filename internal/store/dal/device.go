package dal

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/servekit/license-service/internal/store/generated"
	"github.com/servekit/license-service/internal/store/models"
)

// GetDevice fetches one slot row; gorm.ErrRecordNotFound when the token is
// not in a slot.
func GetDevice(ctx context.Context, tx *gorm.DB, keyHash, deviceToken string) (*models.LicenseDevice, error) {
	d, err := gorm.G[models.LicenseDevice](tx).
		Where(generated.LicenseDevice.KeyHash.Eq(keyHash)).
		Where(generated.LicenseDevice.DeviceToken.Eq(deviceToken)).
		Take(ctx)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// CountDevicesByKeyHash counts occupied slots for the slot judgment.
func CountDevicesByKeyHash(ctx context.Context, tx *gorm.DB, keyHash string) (int64, error) {
	return gorm.G[models.LicenseDevice](tx).
		Where(generated.LicenseDevice.KeyHash.Eq(keyHash)).
		Count(ctx, "*")
}

// ListDevicesByKeyHash returns the slot roster for a key (oldest first).
func ListDevicesByKeyHash(ctx context.Context, tx *gorm.DB, keyHash string) ([]*models.LicenseDevice, error) {
	devices, err := gorm.G[models.LicenseDevice](tx).
		Where(generated.LicenseDevice.KeyHash.Eq(keyHash)).
		Order(generated.LicenseDevice.CreatedAt.Asc()).
		Find(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*models.LicenseDevice, len(devices))
	for i := range devices {
		out[i] = &devices[i]
	}
	return out, nil
}

// TouchDevice updates the mutable annotation columns of a known slot row:
// last_seen_at on every activate, plus fingerprint_id / last_fingerprint_at
// whenever the fingerprint drifted (token-matched rebind). Map arm exists
// because the generic Update is single-column and the rebind column must be
// optional per call (documented exception).
func TouchDevice(_ context.Context, tx *gorm.DB, keyHash, deviceToken, fingerprintID string, now time.Time, rebind bool) error {
	assign := map[string]any{
		"fingerprint_id": fingerprintID,
		"last_seen_at":   now,
	}
	if rebind {
		assign["last_fingerprint_at"] = now
	}
	res := tx.Model(&models.LicenseDevice{}).
		Where(generated.LicenseDevice.KeyHash.Eq(keyHash)).
		Where(generated.LicenseDevice.DeviceToken.Eq(deviceToken)).
		Updates(assign)
	return res.Error
}

// InsertDevice occupies a new slot. The (key_hash, device_token) unique
// index turns concurrent double-inserts into an integrity error, which the
// service layer treats as "retry the slot judgment".
func InsertDevice(ctx context.Context, tx *gorm.DB, d *models.LicenseDevice) error {
	return gorm.G[models.LicenseDevice](tx).Create(ctx, d)
}

// DeleteDevice releases one slot (deactivate / evict / kick); deleting zero
// rows is not an error (idempotent semantics).
func DeleteDevice(ctx context.Context, tx *gorm.DB, keyHash, deviceToken string) error {
	_, err := gorm.G[models.LicenseDevice](tx).
		Where(generated.LicenseDevice.KeyHash.Eq(keyHash)).
		Where(generated.LicenseDevice.DeviceToken.Eq(deviceToken)).
		Delete(ctx)
	return err
}

// DeleteDevicesByKeyHash releases every slot of a key (DeleteKey cascade
// leg).
func DeleteDevicesByKeyHash(ctx context.Context, tx *gorm.DB, keyHash string) error {
	_, err := gorm.G[models.LicenseDevice](tx).
		Where(generated.LicenseDevice.KeyHash.Eq(keyHash)).
		Delete(ctx)
	return err
}
