// Package admin implements the LicenseAdminService domain: key issuance and
// lifecycle, entitlement grants, device management, trial resets, and
// signing-key introspection. Every RPC is Bearer-gated by the admin
// interceptor; this package never sees tokens.
//
// Audit rule (design doc §10.4/§11): mutating RPCs log one admin_audit line.
// Targets use licenseId (keys), device_token (devices), or a truncated
// SHA-256 of the fingerprint (trials — raw fingerprint_id never enters
// logs). Reasons ride along when supplied.
package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"

	licensev1 "github.com/servekit/license-service/gen/license/v1"
	"github.com/servekit/license-service/internal/service/activation"
	"github.com/servekit/license-service/internal/service/cert"
	"github.com/servekit/license-service/internal/store/dal"
	"github.com/servekit/license-service/internal/store/models"
	"github.com/servekit/license-service/pkg/xcodes"
)

// keyGenAttempts bounds the (astronomically unlikely) plaintext-collision
// retry loop; 100-bit entropy makes three attempts ceremonial.
const keyGenAttempts = 3

// Service is the admin domain.
type Service struct {
	db        *gorm.DB
	signer    *cert.Signer
	trialDays int32
}

// New constructs the admin domain.
func New(db *gorm.DB, signer *cert.Signer, trialDays int32) *Service {
	return &Service{db: db, signer: signer, trialDays: trialDays}
}

// ─── helpers ────────────────────────────────────────────────────────────────

// audit emits the admin audit line (target identifiers are log-safe).
func audit(op, target, reason string) {
	args := []any{"op", op, "target", target}
	if reason != "" {
		args = append(args, "reason", reason)
	}
	slog.Info("admin_audit", args...)
}

// fpTarget masks a fingerprint for audit lines: raw fingerprint_id is a
// cross-source trackable machine identifier and never enters logs.
func fpTarget(fp string) string {
	sum := sha256.Sum256([]byte(fp))
	return "trial:" + hex.EncodeToString(sum[:])[:16]
}

// resolveKey maps a dual-form key id (lk_… or 64-hex hash) to the key row,
// translating not-found into the domain error.
func resolveKey(ctx context.Context, tx *gorm.DB, idOrHash string) (*models.LicenseKey, error) {
	k, err := dal.GetKeyByIDOrHash(ctx, tx, idOrHash)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, xcodes.ErrKeyNotFound.New()
	}
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return k, nil
}

// keyToProto assembles the read-side view (never the plaintext key).
func (*Service) keyToProto(ctx context.Context, tx *gorm.DB, k *models.LicenseKey, withDevices bool) (*licensev1.KeyInfo, error) {
	info := &licensev1.KeyInfo{
		LicenseId: k.LicenseID,
		KeyPrefix: k.KeyPrefix,
		Label:     k.Label,
		MaxSlots:  k.MaxSlots,
		Status:    licensev1.KeyStatus(k.Status),
		CreatedAt: timestamppb.New(k.CreatedAt),
	}
	if k.RevokedAt != nil {
		info.RevokedAt = timestamppb.New(*k.RevokedAt)
	}

	ents, err := dal.ListEntitlementsByKeyHash(ctx, tx, k.KeyHash)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	for _, e := range ents {
		ent := &licensev1.EntitlementInfo{
			Module:    licensev1.Module(e.Module),
			Kind:      licensev1.EntitlementKind(e.Kind),
			GrantedAt: timestamppb.New(e.GrantedAt),
		}
		if e.ExpiresAt != nil {
			ent.ExpiresAt = timestamppb.New(*e.ExpiresAt)
		}
		info.Entitlements = append(info.GetEntitlements(), ent)
	}

	devices, err := dal.ListDevicesByKeyHash(ctx, tx, k.KeyHash)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	info.UsedSlots = int32(len(devices))
	if withDevices {
		for _, d := range devices {
			info.Devices = append(info.GetDevices(), deviceToProto(d))
		}
	}
	return info, nil
}

func deviceToProto(d *models.LicenseDevice) *licensev1.DeviceSlotInfo {
	return &licensev1.DeviceSlotInfo{
		DeviceToken:   d.DeviceToken,
		FingerprintId: d.FingerprintID,
		FirstSeenAt:   timestamppb.New(d.CreatedAt),
		LastSeenAt:    timestamppb.New(d.LastSeenAt),
	}
}

// resolveExpiry applies the grant expiry rules (design doc §5.2):
// perpetual forbids any expiry; subscription/trial take exactly one of
// duration_days (extends from max(now, current expiry)) or an absolute
// expires_at. current is nil when no row exists yet.
func resolveExpiry(kind licensev1.EntitlementKind, durationDays int32, expiresAt *timestamppb.Timestamp, current *time.Time, now time.Time) (*time.Time, error) {
	hasDuration := durationDays > 0
	hasAbsolute := expiresAt != nil
	if hasDuration && hasAbsolute {
		return nil, xcodes.ErrBadRequest.New("duration_days and expires_at are mutually exclusive")
	}
	switch kind {
	case licensev1.EntitlementKind_ENTITLEMENT_KIND_PERPETUAL:
		if hasDuration || hasAbsolute {
			return nil, xcodes.ErrBadRequest.New("perpetual grants cannot carry an expiry")
		}
		return nil, nil
	case licensev1.EntitlementKind_ENTITLEMENT_KIND_SUBSCRIPTION,
		licensev1.EntitlementKind_ENTITLEMENT_KIND_TRIAL:
		switch {
		case hasAbsolute:
			t := expiresAt.AsTime()
			return &t, nil
		case hasDuration:
			base := now
			if current != nil && current.After(base) {
				base = *current // extend from the unexpired remainder
			}
			t := base.AddDate(0, 0, int(durationDays))
			return &t, nil
		default:
			return nil, xcodes.ErrBadRequest.New("subscription/trial grants need duration_days or expires_at")
		}
	default:
		return nil, xcodes.ErrBadRequest.New("unknown entitlement kind")
	}
}

// grantRow builds the entitlement row for a validated grant input.
func grantRow(keyHash string, module licensev1.Module, kind licensev1.EntitlementKind, durationDays int32, expiresAt *timestamppb.Timestamp, current *time.Time, now time.Time) (*models.LicenseEntitlement, error) {
	expiry, err := resolveExpiry(kind, durationDays, expiresAt, current, now)
	if err != nil {
		return nil, err
	}
	return &models.LicenseEntitlement{
		KeyHash:   keyHash,
		Module:    int32(module),
		Kind:      int32(kind),
		ExpiresAt: expiry,
		GrantedAt: now,
	}, nil
}

// ─── key lifecycle ──────────────────────────────────────────────────────────

// CreateKey mints a fresh key; the plaintext appears exactly once, in this
// response. Collision on the (entropy-backed) hash retries a few times.
func (s *Service) CreateKey(ctx context.Context, req *licensev1.CreateKeyRequest) (*licensev1.CreateKeyResponse, error) {
	slots := req.GetSlots()
	if slots == 0 {
		slots = 3 // documented default
	}
	if slots < 1 || slots > 100 {
		return nil, xcodes.ErrBadRequest.New("slots must be within [1,100]")
	}

	now := time.Now()
	var plaintext, keyHash string
	var k *models.LicenseKey
	err := s.db.Transaction(func(tx *gorm.DB) error {
		for attempt := 0; attempt < keyGenAttempts; attempt++ {
			candidate, gerr := activation.GenerateKey()
			if gerr != nil {
				return xcodes.ErrInternal.Wrap(gerr)
			}
			norm, nerr := activation.NormalizeKey(candidate)
			if nerr != nil {
				return xcodes.ErrInternal.Wrap(nerr)
			}
			hash := activation.KeyHash(norm)
			row := &models.LicenseKey{
				KeyHash:   hash,
				LicenseID: activation.LicenseID(hash),
				KeyPrefix: norm[:8],
				Label:     req.GetLabel(),
				MaxSlots:  slots,
				Status:    int32(licensev1.KeyStatus_KEY_STATUS_ACTIVE),
			}
			if err := dal.CreateKey(ctx, tx, row); err != nil {
				if isUniqueViolation(err) && attempt < keyGenAttempts-1 {
					continue // entropy collision, try a new key
				}
				return xcodes.ErrInternal.Wrap(err)
			}
			plaintext, keyHash, k = candidate, hash, row
			break
		}
		if k == nil {
			return xcodes.ErrInternal.New("key generation exhausted retry budget")
		}

		for _, g := range req.GetGrants() {
			row, err := grantRow(keyHash, g.GetModule(), g.GetKind(), g.GetDurationDays(), g.GetExpiresAt(), nil, now)
			if err != nil {
				return err
			}
			if err := dal.UpsertEntitlement(ctx, tx, row); err != nil {
				return xcodes.ErrInternal.Wrap(err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	audit("create_key", k.LicenseID, "")

	// Re-read through the view assembler so entitlements are included.
	info, err := s.keyToProto(ctx, s.db, k, false)
	if err != nil {
		return nil, err
	}
	return &licensev1.CreateKeyResponse{Key: info, PlaintextKey: plaintext}, nil
}

// ShowKey returns the full key view including the slot roster.
func (s *Service) ShowKey(ctx context.Context, req *licensev1.ShowKeyRequest) (*licensev1.ShowKeyResponse, error) {
	var info *licensev1.KeyInfo
	err := s.db.Transaction(func(tx *gorm.DB) error {
		k, err := resolveKey(ctx, tx, req.GetKeyId())
		if err != nil {
			return err
		}
		info, err = s.keyToProto(ctx, tx, k, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &licensev1.ShowKeyResponse{Key: info}, nil
}

// ListKeys returns a lightweight roster; status 0 = all.
func (s *Service) ListKeys(ctx context.Context, req *licensev1.ListKeysRequest) (*licensev1.ListKeysResponse, error) {
	limit := req.GetLimit()
	if limit == 0 {
		limit = 100
	}
	keys, err := dal.ListKeys(ctx, s.db, int32(req.GetStatus()), int(limit))
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	resp := &licensev1.ListKeysResponse{}
	for i := range keys {
		info, err := s.keyToProto(ctx, s.db, keys[i], false)
		if err != nil {
			return nil, err
		}
		resp.Keys = append(resp.GetKeys(), info)
	}
	return resp, nil
}

// UpdateKey applies optional label/slots updates.
func (s *Service) UpdateKey(ctx context.Context, req *licensev1.UpdateKeyRequest) (*licensev1.UpdateKeyResponse, error) {
	if req.Label == nil && req.Slots == nil {
		return nil, xcodes.ErrBadRequest.New("nothing to update")
	}
	if req.Slots != nil && (*req.Slots < 1 || *req.Slots > 100) {
		return nil, xcodes.ErrBadRequest.New("slots must be within [1,100]")
	}

	var info *licensev1.KeyInfo
	err := s.db.Transaction(func(tx *gorm.DB) error {
		k, err := resolveKey(ctx, tx, req.GetKeyId())
		if err != nil {
			return err
		}
		if req.Label != nil {
			if err := dal.UpdateKeyLabel(ctx, tx, k.KeyHash, *req.Label); err != nil {
				return xcodes.ErrInternal.Wrap(err)
			}
		}
		if req.Slots != nil {
			if err := dal.UpdateKeySlots(ctx, tx, k.KeyHash, *req.Slots); err != nil {
				return xcodes.ErrInternal.Wrap(err)
			}
		}
		fresh, err := dal.GetKeyByKeyHash(ctx, tx, k.KeyHash)
		if err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		info, err = s.keyToProto(ctx, tx, fresh, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	audit("update_key", info.GetLicenseId(), "")
	return &licensev1.UpdateKeyResponse{Key: info}, nil
}

// RevokeKey freezes the key (rows kept; slots reattach on unrevoke).
func (s *Service) RevokeKey(ctx context.Context, req *licensev1.RevokeKeyRequest) (*licensev1.RevokeKeyResponse, error) {
	now := time.Now()
	var info *licensev1.KeyInfo
	err := s.db.Transaction(func(tx *gorm.DB) error {
		k, err := resolveKey(ctx, tx, req.GetKeyId())
		if err != nil {
			return err
		}
		if err := dal.SetKeyStatus(ctx, tx, k.KeyHash, int32(licensev1.KeyStatus_KEY_STATUS_REVOKED), &now); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		fresh, err := dal.GetKeyByKeyHash(ctx, tx, k.KeyHash)
		if err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		info, err = s.keyToProto(ctx, tx, fresh, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	audit("revoke_key", info.GetLicenseId(), req.GetReason())
	return &licensev1.RevokeKeyResponse{Key: info}, nil
}

// UnrevokeKey reactivates the key; slots reattach as they were.
func (s *Service) UnrevokeKey(ctx context.Context, req *licensev1.UnrevokeKeyRequest) (*licensev1.UnrevokeKeyResponse, error) {
	var info *licensev1.KeyInfo
	err := s.db.Transaction(func(tx *gorm.DB) error {
		k, err := resolveKey(ctx, tx, req.GetKeyId())
		if err != nil {
			return err
		}
		if err := dal.SetKeyStatus(ctx, tx, k.KeyHash, int32(licensev1.KeyStatus_KEY_STATUS_ACTIVE), nil); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		fresh, err := dal.GetKeyByKeyHash(ctx, tx, k.KeyHash)
		if err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		info, err = s.keyToProto(ctx, tx, fresh, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	audit("unrevoke_key", info.GetLicenseId(), "")
	return &licensev1.UnrevokeKeyResponse{Key: info}, nil
}

// DeleteKey physically removes the key and, in the same transaction, its
// entitlements and devices (app-level cascade — FKs are disabled by dbx).
func (s *Service) DeleteKey(ctx context.Context, req *licensev1.DeleteKeyRequest) (*licensev1.DeleteKeyResponse, error) {
	if req.Confirm == nil || !*req.Confirm {
		return nil, xcodes.ErrBadRequest.New("physical delete requires confirm=true")
	}
	var licenseID string
	err := s.db.Transaction(func(tx *gorm.DB) error {
		k, err := resolveKey(ctx, tx, req.GetKeyId())
		if err != nil {
			return err
		}
		licenseID = k.LicenseID
		if err := dal.DeleteEntitlementsByKeyHash(ctx, tx, k.KeyHash); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		if err := dal.DeleteDevicesByKeyHash(ctx, tx, k.KeyHash); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		if err := dal.DeleteKey(ctx, tx, k.KeyHash); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	audit("delete_key", licenseID, "")
	return &licensev1.DeleteKeyResponse{}, nil
}

// ─── entitlements ───────────────────────────────────────────────────────────

// GrantModule upserts one module entitlement; expiry follows the
// duration/absolute rules against the current row.
func (s *Service) GrantModule(ctx context.Context, req *licensev1.GrantModuleRequest) (*licensev1.GrantModuleResponse, error) {
	now := time.Now()
	var out *licensev1.EntitlementInfo
	err := s.db.Transaction(func(tx *gorm.DB) error {
		k, err := resolveKey(ctx, tx, req.GetKeyId())
		if err != nil {
			return err
		}
		var current *time.Time
		if existing, gerr := dal.GetEntitlement(ctx, tx, k.KeyHash, int32(req.GetModule())); gerr == nil {
			current = existing.ExpiresAt
		} else if !errors.Is(gerr, gorm.ErrRecordNotFound) {
			return xcodes.ErrInternal.Wrap(gerr)
		}
		row, err := grantRow(k.KeyHash, req.GetModule(), req.GetKind(), req.GetDurationDays(), req.GetExpiresAt(), current, now)
		if err != nil {
			return err
		}
		if err := dal.UpsertEntitlement(ctx, tx, row); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		out = &licensev1.EntitlementInfo{
			Module:    req.GetModule(),
			Kind:      req.GetKind(),
			GrantedAt: timestamppb.New(row.GrantedAt),
		}
		if row.ExpiresAt != nil {
			out.ExpiresAt = timestamppb.New(*row.ExpiresAt)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	audit("grant_module", fmt.Sprintf("%s:%s", req.GetKeyId(), req.GetModule().String()), "")
	return &licensev1.GrantModuleResponse{Entitlement: out}, nil
}

// RevokeModule removes one module entitlement; the next issuance drops it.
func (s *Service) RevokeModule(ctx context.Context, req *licensev1.RevokeModuleRequest) (*licensev1.RevokeModuleResponse, error) {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		k, err := resolveKey(ctx, tx, req.GetKeyId())
		if err != nil {
			return err
		}
		if err := dal.DeleteEntitlement(ctx, tx, k.KeyHash, int32(req.GetModule())); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	audit("revoke_module", fmt.Sprintf("%s:%s", req.GetKeyId(), req.GetModule().String()), req.GetReason())
	return &licensev1.RevokeModuleResponse{}, nil
}

// ─── devices ────────────────────────────────────────────────────────────────

// ListKeyDevices returns the slot roster.
func (s *Service) ListKeyDevices(ctx context.Context, req *licensev1.ListKeyDevicesRequest) (*licensev1.ListKeyDevicesResponse, error) {
	resp := &licensev1.ListKeyDevicesResponse{}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		k, err := resolveKey(ctx, tx, req.GetKeyId())
		if err != nil {
			return err
		}
		devices, err := dal.ListDevicesByKeyHash(ctx, tx, k.KeyHash)
		if err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		for _, d := range devices {
			resp.Devices = append(resp.GetDevices(), deviceToProto(d))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// KickDevice force-releases one slot (support-side eviction).
func (s *Service) KickDevice(ctx context.Context, req *licensev1.KickDeviceRequest) (*licensev1.KickDeviceResponse, error) {
	kicked := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		k, err := resolveKey(ctx, tx, req.GetKeyId())
		if err != nil {
			return err
		}
		before, err := dal.CountDevicesByKeyHash(ctx, tx, k.KeyHash)
		if err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		if err := dal.DeleteDevice(ctx, tx, k.KeyHash, req.GetDeviceToken()); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		after, err := dal.CountDevicesByKeyHash(ctx, tx, k.KeyHash)
		if err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		kicked = after < before
		return nil
	})
	if err != nil {
		return nil, err
	}
	audit("kick_device", fmt.Sprintf("%s:%s", req.GetKeyId(), req.GetDeviceToken()), req.GetReason())
	return &licensev1.KickDeviceResponse{Kicked: kicked}, nil
}

// ─── trials ─────────────────────────────────────────────────────────────────

// ShowTrial returns the fingerprint's ledger (module 0 = all modules),
// expiry derived at the configured trial length.
func (s *Service) ShowTrial(ctx context.Context, req *licensev1.ShowTrialRequest) (*licensev1.ShowTrialResponse, error) {
	trials, err := dal.ListTrialsByFingerprint(ctx, s.db, req.GetFingerprintId())
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	resp := &licensev1.ShowTrialResponse{}
	for _, tr := range trials {
		if req.GetModule() != licensev1.Module_MODULE_UNSPECIFIED && tr.Module != int32(req.GetModule()) {
			continue
		}
		resp.Trials = append(resp.GetTrials(), &licensev1.TrialInfo{
			FingerprintId:    tr.FingerprintID,
			Module:           licensev1.Module(tr.Module),
			StartedAt:        timestamppb.New(tr.StartedAt),
			ExpiresAt:        timestamppb.New(tr.StartedAt.AddDate(0, 0, int(s.trialDays))),
			FirstDeviceToken: tr.FirstDeviceToken,
		})
	}
	return resp, nil
}

// ResetTrial deletes the ledger row — the only manual reset channel; reason
// is mandatory (proto) and lands in the audit log.
func (s *Service) ResetTrial(ctx context.Context, req *licensev1.ResetTrialRequest) (*licensev1.ResetTrialResponse, error) {
	reset := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		got, err := dal.GetTrial(ctx, tx, req.GetFingerprintId(), int32(req.GetModule()))
		switch {
		case err == nil:
			_ = got // row existence is all we need
			if err := dal.DeleteTrial(ctx, tx, req.GetFingerprintId(), int32(req.GetModule())); err != nil {
				return xcodes.ErrInternal.Wrap(err)
			}
			reset = true
			return nil
		case errors.Is(err, gorm.ErrRecordNotFound):
			return nil
		default:
			return xcodes.ErrInternal.Wrap(err)
		}
	})
	if err != nil {
		return nil, err
	}
	audit("reset_trial", fpTarget(req.GetFingerprintId())+":"+req.GetModule().String(), req.GetReason())
	return &licensev1.ResetTrialResponse{Reset_: reset}, nil
}

// ─── signing ────────────────────────────────────────────────────────────────

// ShowPubKey exposes the signing public keys for pinning into client key
// tables: every configured key, sorted by kid, plus the active kid.
func (s *Service) ShowPubKey(_ context.Context, _ *licensev1.ShowPubKeyRequest) (*licensev1.ShowPubKeyResponse, error) {
	resp := &licensev1.ShowPubKeyResponse{ActiveKeyId: s.signer.ActiveKeyID()}
	pubs := s.signer.PublicKeys()
	kids := make([]string, 0, len(pubs))
	for kid := range pubs {
		kids = append(kids, kid)
	}
	sort.Strings(kids)
	for _, kid := range kids {
		resp.Keys = append(resp.GetKeys(),
			&licensev1.SigningKeyInfo{KeyId: kid, PublicKeyB64: pubs[kid]})
	}
	return resp, nil
}

// isUniqueViolation reports PostgreSQL unique-index violations (SQLSTATE
// 23505) — the only retryable error in the key-generation loop.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
