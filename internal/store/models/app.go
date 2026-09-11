// Package models: LicenseApp — calling-application registry for the
// licensing platform.
package models

import "time"

// LicenseApp is the tenant's GATE row on the licensing platform (a business
// system identity, NOT an end-user license). Since the ④ window close the
// data-plane caller presents the trusted x-tenant-key and this row is
// purely the GATE: license keys/devices/trials/entitlements are global
// (no tenant dimension), so the gate row is the only per-tenant artifact.
// Mirrors the messaging/storage platform app pattern (the minted secret
// only satisfies the not-null column — PLAINTEXT, internal-trust posture).
//
// Hard row (no DeletedAt), per licensing convention: app deletion is final
// and the app_key becomes reusable.
type LicenseApp struct {
	ID     int64  `gorm:"primaryKey"`
	AppKey string `gorm:"column:app_key;size:64;uniqueIndex:uq_license_apps_app_key;not null"`
	Name   string `gorm:"size:200;not null"`
	// TenantKey maps the app to its tenant. Nullable transition: NULL = not
	// yet backfilled; the data plane falls back to the app_key literal (T10
	// 总装 clears the empties). Unique —
	// one gate row per tenant (shared by the trusted first-sight lazy
	// upsert and the legacy mapping).
	TenantKey *string `gorm:"size:16;column:tenant_key;uniqueIndex:uq_license_apps_tenant_key"`
	// Disabled apps fail every data-plane call immediately.
	Disabled  bool      `gorm:"not null;default:false"`
	CreatedAt time.Time `gorm:"column:created_at;not null;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null;autoUpdateTime"`
}

// TableName pins the table name.
func (LicenseApp) TableName() string { return "license_apps" }

// --- tenant_key helpers (nullable-column ergonomics, phase ③ window) ---

// TenantKeyOf dereferences a nullable tenant_key column; nil → "".
func TenantKeyOf(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// TenantKeyPtr boxes a tenant key; "" → nil (writes NULL — the
// not-yet-backfilled marker).
func TenantKeyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// AppTenantKey resolves the tenant an app row maps to: the tenant_key column
// when backfilled, else the app_key literal (phase ③ window fallback —
// T10 总装 clears the empty columns).
func AppTenantKey(a *LicenseApp) string {
	if a == nil {
		return ""
	}
	if tk := TenantKeyOf(a.TenantKey); tk != "" {
		return tk
	}
	return a.AppKey
}
