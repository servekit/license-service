// Package models: LicenseApp — calling-application registry for the
// licensing platform.
package models

import "time"

// LicenseApp is one calling application of the licensing platform (a business
// system identity, NOT an end-user license). Data-plane callers present
// (app_key, app_secret) as x-app-key / x-app-secret metadata. Mirrors the
// messaging/storage platform app pattern (secret stored PLAINTEXT —
// internal-trust posture).
//
// Hard row (no DeletedAt), per licensing convention: app deletion is final
// and the app_key becomes reusable.
type LicenseApp struct {
	ID        int64  `gorm:"primaryKey"`
	AppKey    string `gorm:"column:app_key;size:64;uniqueIndex:uq_license_apps_app_key;not null"`
	AppSecret string `gorm:"column:app_secret;size:128;not null"`
	Name      string `gorm:"size:200;not null"`
	// Disabled apps fail every data-plane call immediately.
	Disabled  bool      `gorm:"not null;default:false"`
	CreatedAt time.Time `gorm:"column:created_at;not null;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null;autoUpdateTime"`
}

// TableName pins the table name.
func (LicenseApp) TableName() string { return "license_apps" }
