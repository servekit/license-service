package dal

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/servekit/license-service/internal/store/generated"
	"github.com/servekit/license-service/internal/store/models"
)

// GetTrial fetches the (fingerprint_id, module) ledger row;
// gorm.ErrRecordNotFound when no trial exists.
func GetTrial(ctx context.Context, tx *gorm.DB, fingerprintID string, module int32) (*models.LicenseTrial, error) {
	tr, err := gorm.G[models.LicenseTrial](tx).
		Where(generated.LicenseTrial.FingerprintID.Eq(fingerprintID)).
		Where(generated.LicenseTrial.Module.Eq(module)).
		Take(ctx)
	if err != nil {
		return nil, err
	}
	return &tr, nil
}

// InsertTrialOnConflictDoNothing inserts a trial row, ignoring the insert
// when the (fingerprint_id, module) row already exists — concurrent trial
// starts collapse to exactly one row, and the caller re-reads to recover the
// winning started_at.
func InsertTrialOnConflictDoNothing(_ context.Context, tx *gorm.DB, tr *models.LicenseTrial) error {
	res := tx.Model(&models.LicenseTrial{}).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "fingerprint_id"}, {Name: "module"}},
			DoNothing: true,
		}).
		Where(generated.LicenseTrial.FingerprintID.Eq(tr.FingerprintID)).
		Where(generated.LicenseTrial.Module.Eq(tr.Module)).
		Create(tr)
	return res.Error
}

// ListTrialsByFingerprint returns the full trial ledger of a fingerprint
// (all modules, including expired entries — keyless certs carry them all).
func ListTrialsByFingerprint(ctx context.Context, tx *gorm.DB, fingerprintID string) ([]*models.LicenseTrial, error) {
	trials, err := gorm.G[models.LicenseTrial](tx).
		Where(generated.LicenseTrial.FingerprintID.Eq(fingerprintID)).
		Order(generated.LicenseTrial.Module.Asc()).
		Find(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*models.LicenseTrial, len(trials))
	for i := range trials {
		out[i] = &trials[i]
	}
	return out, nil
}

// DeleteTrial removes the ledger row (admin ResetTrial — the only manual
// reset channel).
func DeleteTrial(ctx context.Context, tx *gorm.DB, fingerprintID string, module int32) error {
	_, err := gorm.G[models.LicenseTrial](tx).
		Where(generated.LicenseTrial.FingerprintID.Eq(fingerprintID)).
		Where(generated.LicenseTrial.Module.Eq(module)).
		Delete(ctx)
	return err
}
