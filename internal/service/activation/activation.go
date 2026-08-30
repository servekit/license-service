// Package activation implements the client-facing activation domain: the
// idempotent Activate converger (first activation, heartbeat, fingerprint
// rebind, refresh, evict-retry), Deactivate, and keyless TrialStart — plus
// the per-identity Redis rate limiter with 409-refund semantics.
//
// Business rules come from docs/design.md §7; the A1–A12 scenario table
// there is mirrored 1:1 in activation_test.go.
package activation

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"

	licensev1 "github.com/servekit/license-service/gen/license/v1"
	"github.com/servekit/license-service/internal/service/cert"
	"github.com/servekit/license-service/internal/store/dal"
	"github.com/servekit/license-service/internal/store/models"
	"github.com/servekit/license-service/pkg/xcodes"
)

// Service is the activation domain. Resources are injected by the service
// root; this struct holds no lifecycle of its own.
type Service struct {
	db        *gorm.DB
	rdb       *redis.Client
	signer    *cert.Signer
	trialDays int32
	limiter   rateLimiter
}

// New constructs the activation domain.
func New(db *gorm.DB, rdb *redis.Client, signer *cert.Signer, trialDays int32) *Service {
	return &Service{
		db:        db,
		rdb:       rdb,
		signer:    signer,
		trialDays: trialDays,
		limiter:   rateLimiter{rdb: rdb},
	}
}

// firstKey picks the first non-empty key field (the client sends `key`; the
// original docs wrote `licenseKey` — both are accepted).
func firstKey(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// normalizeRequestKey resolves the dual-name key fields to the key hash.
func normalizeRequestKey(a, b string) (hash string, err error) {
	norm, err := NormalizeKey(firstKey(a, b))
	if err != nil {
		return "", err
	}
	return KeyHash(norm), nil
}

// Activate is the idempotent converger. Order (design doc §7.1):
// normalize → hash → rate limit → key lookup (401/403) → evict delete →
// same-transaction slot judgment (FOR UPDATE serialize) → assemble
// entitlements ∪ trial ledger → sign with fresh certId/issuedAt → 200.
// A slot-limit rejection refunds the rate-limit unit so the user can retry
// immediately after choosing a device to evict.
func (s *Service) Activate(ctx context.Context, req *licensev1.ActivateRequest) (*licensev1.ActivateResponse, error) {
	keyHash, err := normalizeRequestKey(req.GetKey(), req.GetLicenseKey())
	if err != nil {
		return nil, err
	}
	fingerprint := req.GetFingerprintId()
	token := req.GetDeviceToken()

	refund, err := s.limiter.check(ctx, "key:"+keyHash, token)
	if err != nil {
		return nil, err
	}

	var (
		k       *models.LicenseKey
		devices []*models.LicenseDevice
		ents    []*models.LicenseEntitlement
		trials  []*models.LicenseTrial
	)
	txErr := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		k, err = dal.GetKeyForUpdate(ctx, tx, keyHash)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return xcodes.ErrKeyNotFound.New()
		}
		if err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		if k.Status == int32(licensev1.KeyStatus_KEY_STATUS_REVOKED) {
			return xcodes.ErrKeyRevoked.New() // A9: includes heartbeats from in-slot devices
		}

		if err := s.occupySlot(ctx, tx, keyHash, fingerprint, token, req.GetEvictDeviceToken(), k.MaxSlots); err != nil {
			return err
		}

		if devices, err = dal.ListDevicesByKeyHash(ctx, tx, keyHash); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		if ents, err = dal.ListEntitlementsByKeyHash(ctx, tx, keyHash); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		if trials, err = dal.ListTrialsByFingerprint(ctx, tx, fingerprint); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		return nil
	})
	if txErr != nil {
		// 409 refunds the rate-limit unit (design doc §7.5).
		if errors.Is(txErr, xcodes.ErrSlotLimit.New()) && refund != nil {
			refund()
		}
		return nil, txErr
	}

	payload, sig, err := s.signCert(&licenseIDRef{hash: keyHash}, token, fingerprint, ents, trials)
	if err != nil {
		return nil, err
	}
	return &licensev1.ActivateResponse{
		Payload:   payload,
		Signature: sig,
		Slots:     slotsSummary(k.MaxSlots, devices),
	}, nil
}

// occupySlot applies the slot judgment inside the caller's transaction: an
// existing (key_hash, device_token) row is reused (A2 heartbeat / A3 token-
// matched rebind); a fresh token counts against max_slots and inserts when
// there is room (A1) or produces the detailed SLOT_LIMIT error with the
// roster (A5/A12).
func (*Service) occupySlot(ctx context.Context, tx *gorm.DB, keyHash, fingerprint, token, evictToken string, maxSlots int32) error {
	now := time.Now()
	// A6/A7: evict the chosen device first; deleting zero rows is fine.
	if evictToken != "" {
		if err := dal.DeleteDevice(ctx, tx, keyHash, evictToken); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
	}

	d, err := dal.GetDevice(ctx, tx, keyHash, token)
	switch {
	case err == nil:
		rebind := d.FingerprintID != fingerprint // A3: token matched, fingerprint drifted
		if err := dal.TouchDevice(ctx, tx, keyHash, token, fingerprint, now, rebind); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		return nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		n, err := dal.CountDevicesByKeyHash(ctx, tx, keyHash)
		if err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		if n >= int64(maxSlots) {
			roster, lerr := dal.ListDevicesByKeyHash(ctx, tx, keyHash)
			if lerr != nil {
				return xcodes.ErrInternal.Wrap(lerr)
			}
			info := &licensev1.SlotLimitInfo{MaxSlots: maxSlots}
			for _, dev := range roster {
				info.Devices = append(info.GetDevices(), deviceToProto(dev))
			}
			return xcodes.WithDetails(
				xcodes.ErrSlotLimit.New("all device slots are in use, evict one and retry"),
				info,
			)
		}
		row := &models.LicenseDevice{
			KeyHash:       keyHash,
			DeviceToken:   token,
			FingerprintID: fingerprint,
			LastSeenAt:    now,
		}
		if err := dal.InsertDevice(ctx, tx, row); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		return nil
	default:
		return xcodes.ErrInternal.Wrap(err)
	}
}

// Deactivate releases the caller's own slot. Idempotent: a device that was
// never in a slot (or a key that does not exist at all) still returns 200
// with released=false; revoked keys are also allowed (cleanup is harmless).
func (s *Service) Deactivate(ctx context.Context, req *licensev1.DeactivateRequest) (*licensev1.DeactivateResponse, error) {
	keyHash, err := normalizeRequestKey(req.GetKey(), req.GetLicenseKey())
	if err != nil {
		return nil, err
	}
	token := req.GetDeviceToken()

	refund, err := s.limiter.check(ctx, "key:"+keyHash, token)
	if err != nil {
		return nil, err
	}
	// Deactivate never returns 409; consume the unit unconditionally.
	_ = refund

	var released bool
	err = s.db.Transaction(func(tx *gorm.DB) error {
		// Key lookup is advisory only: missing key or revoked key still
		// returns released=false (deactivate is always 200; cleanup of a
		// slot the caller cannot occupy is harmless).
		if _, kerr := dal.GetKeyByKeyHash(ctx, tx, keyHash); kerr != nil &&
			!errors.Is(kerr, gorm.ErrRecordNotFound) {
			return xcodes.ErrInternal.Wrap(kerr)
		}
		before, err := dal.CountDevicesByKeyHash(ctx, tx, keyHash)
		if err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		if err := dal.DeleteDevice(ctx, tx, keyHash, token); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		after, err := dal.CountDevicesByKeyHash(ctx, tx, keyHash)
		if err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		released = after < before
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &licensev1.DeactivateResponse{Released: released}, nil
}

// TrialStart starts (or idempotently resumes) a keyless trial for
// (fingerprint_id, module); with license_key set it additionally runs the
// full key discipline and merges the trial ledger into a keyed cert
// (design doc §7.4). Reinstalling with a fresh device_token does not reset
// the trial — identity is the fingerprint.
func (s *Service) TrialStart(ctx context.Context, req *licensev1.TrialStartRequest) (*licensev1.TrialStartResponse, error) {
	fingerprint := req.GetFingerprintId()
	token := req.GetDeviceToken()
	module, err := moduleFromWire(req.GetModule())
	if err != nil {
		return nil, err
	}

	refund, err := s.limiter.check(ctx, "fp:"+fingerprint, token)
	if err != nil {
		return nil, err
	}

	var (
		keyHash      string
		keyed        bool
		trial        *models.LicenseTrial
		alreadyStart bool
		ents         []*models.LicenseEntitlement
	)
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if raw := req.GetLicenseKey(); raw != "" {
			hash, nerr := normalizeRequestKey(raw, "")
			if nerr != nil {
				return nerr
			}
			k, kerr := dal.GetKeyForUpdate(ctx, tx, hash)
			if errors.Is(kerr, gorm.ErrRecordNotFound) {
				return xcodes.ErrKeyNotFound.New()
			}
			if kerr != nil {
				return xcodes.ErrInternal.Wrap(kerr)
			}
			if k.Status == int32(licensev1.KeyStatus_KEY_STATUS_REVOKED) {
				return xcodes.ErrKeyRevoked.New()
			}
			// ALREADY_ENTITLED: the key holds a still-valid entitlement for
			// the requested module — a trial would be redundant.
			keyEnts, lerr := dal.ListEntitlementsByKeyHash(ctx, tx, hash)
			if lerr != nil {
				return xcodes.ErrInternal.Wrap(lerr)
			}
			for _, e := range keyEnts {
				if e.Module != int32(module) {
					continue
				}
				if e.ExpiresAt == nil || e.ExpiresAt.After(time.Now()) {
					return xcodes.ErrAlreadyEntitled.New()
				}
			}
			if serr := s.occupySlot(ctx, tx, hash, fingerprint, token, "", k.MaxSlots); serr != nil {
				return serr
			}
			keyHash, keyed = hash, true
			ents = keyEnts
		}

		// Ledger: existing row wins (idempotent resume); concurrent starts
		// collapse via ON CONFLICT DO NOTHING + re-read.
		existing, gerr := dal.GetTrial(ctx, tx, fingerprint, int32(module))
		switch {
		case gerr == nil:
			trial, alreadyStart = existing, true
		case errors.Is(gerr, gorm.ErrRecordNotFound):
			row := &models.LicenseTrial{
				FingerprintID:    fingerprint,
				Module:           int32(module),
				StartedAt:        time.Now(),
				FirstDeviceToken: token,
			}
			if ierr := dal.InsertTrialOnConflictDoNothing(ctx, tx, row); ierr != nil {
				return xcodes.ErrInternal.Wrap(ierr)
			}
			reread, rerr := dal.GetTrial(ctx, tx, fingerprint, int32(module))
			if rerr != nil {
				return xcodes.ErrInternal.Wrap(rerr)
			}
			trial = reread
		default:
			return xcodes.ErrInternal.Wrap(gerr)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, xcodes.ErrSlotLimit.New()) && refund != nil {
			refund()
		}
		return nil, err
	}

	trials := []*models.LicenseTrial{trial}
	// Keyless certs carry the FULL ledger (all modules, expired included) so
	// the client can derive trialUsed; keyed certs union the ledger under
	// key priority (handled by signCert).
	if !keyed {
		all, lerr := dal.ListTrialsByFingerprint(ctx, s.db, fingerprint)
		if lerr != nil {
			return nil, xcodes.ErrInternal.Wrap(lerr)
		}
		trials = all
	}

	var idRef *licenseIDRef
	if keyed {
		idRef = &licenseIDRef{hash: keyHash}
	}
	payload, sig, err := s.signCert(idRef, token, fingerprint, ents, trials)
	if err != nil {
		return nil, err
	}
	return &licensev1.TrialStartResponse{
		Payload:        payload,
		Signature:      sig,
		AlreadyStarted: alreadyStart,
	}, nil
}

// licenseIDRef marks a keyed cert (nil = keyless trial cert).
type licenseIDRef struct{ hash string }

// signCert assembles the cert payload from key entitlements and the trial
// ledger — same-module key entries take priority, and the trial ledger is
// ALWAYS merged (a missing merge would let key certs and trial certs evict
// each other in the client's single-slot credential store). Expired entries
// stay in (A8: the client judges expiry; the renewal channel stays open).
// certId is a fresh UUID and issuedAt is the current server time on every
// issuance — never reuse a timestamp.
func (s *Service) signCert(keyRef *licenseIDRef, deviceToken, fingerprint string, ents []*models.LicenseEntitlement, trials []*models.LicenseTrial) (payload, signatureB64 string, err error) {
	entMap := map[string]cert.Entitlement{}
	for _, tr := range trials {
		name := moduleWireName(moduleFromInt32(tr.Module))
		if name == "" {
			continue
		}
		expiry := tr.StartedAt.AddDate(0, 0, int(s.trialDays))
		entMap[name] = cert.Entitlement{Kind: cert.KindTrial, ExpiresAt: &expiry}
	}
	for _, e := range ents {
		name := moduleWireName(moduleFromInt32(e.Module))
		kind, ok := kindFromInt32(e.Kind)
		if name == "" || !ok {
			continue
		}
		entMap[name] = cert.Entitlement{Kind: cert.Kind(kind), ExpiresAt: e.ExpiresAt}
	}

	p := &cert.Payload{
		V:             1,
		CertID:        uuid.NewString(),
		DeviceToken:   deviceToken,
		FingerprintID: fingerprint,
		IssuedAt:      time.Now(),
		Entitlements:  entMap,
	}
	if keyRef != nil {
		id := LicenseID(keyRef.hash)
		p.LicenseID = &id
	}
	return s.signer.Sign(p)
}

// slotsSummary shapes the activate response roster.
func slotsSummary(maxSlots int32, devices []*models.LicenseDevice) *licensev1.SlotSummary {
	summary := &licensev1.SlotSummary{Used: int32(len(devices)), Max: maxSlots}
	for _, d := range devices {
		summary.Devices = append(summary.GetDevices(), deviceToProto(d))
	}
	return summary
}

// deviceToProto maps a slot row; first_seen_at comes from CreatedAt (the
// anti-redundancy fold), name stays unset (no name source today).
func deviceToProto(d *models.LicenseDevice) *licensev1.DeviceSlotInfo {
	return &licensev1.DeviceSlotInfo{
		DeviceToken:   d.DeviceToken,
		FingerprintId: d.FingerprintID,
		FirstSeenAt:   timestamppb.New(d.CreatedAt),
		LastSeenAt:    timestamppb.New(d.LastSeenAt),
	}
}
