package models

import "time"

// LicenseEntitlement is one module row per key. Free modules never enter
// the table. The kind/expires_at NULL pairing is a hard server-side
// invariant (perpetual ⇒ NULL, subscription/trial ⇒ non-NULL), enforced on
// both the admin grant path and activation.
type LicenseEntitlement struct {
	KeyHash string `gorm:"column:key_hash;size:64;not null;uniqueIndex:uq_license_entitlements_key_module,priority:1"`
	// Module stores a licensev1.Module value (1=downloads 2=tools).
	Module int32 `gorm:"not null;uniqueIndex:uq_license_entitlements_key_module,priority:2"`
	// Kind stores a licensev1.EntitlementKind value (1=perpetual
	// 2=subscription 3=trial).
	Kind      int32 `gorm:"not null"`
	ExpiresAt *time.Time
	// GrantedAt is the business timestamp of the latest grant/modification.
	GrantedAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName returns the explicit table name.
func (LicenseEntitlement) TableName() string { return "license_entitlements" }
