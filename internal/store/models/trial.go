package models

import "time"

// LicenseTrial is the keyless trial ledger, independent of the key system.
// Uniqueness on (fingerprint_id, module) means reinstalling the app with a
// fresh device_token does NOT reset a trial; admin ResetTrial deletes the
// row (hard) as the only manual reset channel.
type LicenseTrial struct {
	FingerprintID string `gorm:"column:fingerprint_id;size:67;not null;uniqueIndex:uq_license_trials_fp_module,priority:1"`
	// Module stores a licensev1.Module value.
	Module int32 `gorm:"not null;uniqueIndex:uq_license_trials_fp_module,priority:2"`
	// StartedAt anchors expiry (started_at + trial days) on the server clock.
	StartedAt time.Time
	// FirstDeviceToken is audit-only.
	FirstDeviceToken string `gorm:"column:first_device_token;size:36;not null"`
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// TableName returns the explicit table name.
func (LicenseTrial) TableName() string { return "license_trials" }
