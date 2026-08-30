// Package models defines the GORM table structs for license-service.
//
// Conventions (gorm-cli-development skill): every model declares an explicit
// surrogate ID (int64 auto-increment) plus CreatedAt/UpdatedAt; business keys
// are named composite unique indexes; enums are proto-owned and stored as
// int32 (cast at the service-layer store boundary); nullable columns are
// pointers; tags carry the full migration contract (size/not null/default).
//
// DEVIATION — no DeletedAt: license tables are hard rows by design
// (docs/design.md §4). Slot freeing (deactivate/evict), trial resets, and
// entitlement upserts all rely on the physical absence of a row to free its
// unique-index slot; soft-deleted rows would keep occupying (key_hash,
// device_token) / (fingerprint_id, module) and break re-activation and
// trial restarts. Key revocation is a Status soft state, not a soft delete.
package models

import "time"

// LicenseKey is the key master row. The plaintext key is NEVER stored —
// only its SHA-256 hash, the derived license_id, and an 8-char prefix for
// support-side comparison.
type LicenseKey struct {
	KeyHash   string `gorm:"column:key_hash;size:64;not null;uniqueIndex:uq_license_keys_key_hash"`
	LicenseID string `gorm:"column:license_id;size:35;not null;uniqueIndex"`
	KeyPrefix string `gorm:"column:key_prefix;size:8;not null"`
	Label     string `gorm:"size:200"`
	// MaxSlots bounds concurrent devices (>= 1, service-checked).
	MaxSlots int32 `gorm:"not null;default:3"`
	// Status stores a licensev1.KeyStatus value (1=active 2=revoked).
	Status int32 `gorm:"not null;default:1;index"`
	// RevokedAt is set exactly while Status=revoked; cleared on unrevoke.
	RevokedAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName returns the explicit table name (skill convention: the service
// prefix lives in the struct-derived name).
func (LicenseKey) TableName() string { return "license_keys" }
