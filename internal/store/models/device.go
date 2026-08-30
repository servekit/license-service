package models

import "time"

// LicenseDevice is one device occupying one slot of a key. device_token is
// the slot identity anchor (client-side stable UUID); fingerprint_id is a
// mutable annotation updated on every token-matched activate (auto rebind).
// Hard rows: no DeletedAt — releasing a slot deletes the row (see the
// package comment for why soft delete would break slot re-acquisition).
type LicenseDevice struct {
	KeyHash string `gorm:"column:key_hash;size:64;not null;uniqueIndex:uq_license_devices_key_token,priority:1"`
	// DeviceToken is a client-generated UUID v4, never re-rolled.
	DeviceToken string `gorm:"column:device_token;size:36;not null;uniqueIndex:uq_license_devices_key_token,priority:2"`
	// FingerprintID is "v1." + 64 hex chars.
	FingerprintID     string    `gorm:"column:fingerprint_id;size:67;not null"`
	LastSeenAt        time.Time // latest successful activate (heartbeats funnel here)
	LastFingerprintAt *time.Time
	// CreatedAt doubles as first_seen_at (proto DeviceSlotInfo.first_seen_at
	// maps from it — no redundant FirstSeenAt column).
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName returns the explicit table name.
func (LicenseDevice) TableName() string { return "license_devices" }
