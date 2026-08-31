package activation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/stretchr/testify/require"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/redisx"

	licensev1 "github.com/servekit/license-service/gen/license/v1"
	"github.com/servekit/license-service/internal/service/cert"
	"github.com/servekit/license-service/internal/store/dal"
	"github.com/servekit/license-service/internal/store/models"
	"github.com/servekit/license-service/pkg/xcodes"
)

// testSeed is the public RFC 8032 Ed25519 TEST1 vector — test material only.
const testSeed = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"

type harness struct {
	svc *Service
	db  *gorm.DB
	rdb *redis.Client
}

// defaultHarnessOptions mirrors the config default tags (pkg/config) — the
// domain no longer substitutes defaults itself.
func defaultHarnessOptions(trialDays int32) Options {
	return Options{
		TrialDays:     trialDays,
		RateKeyPrefix: "license:rate",
		RateWindow:    time.Minute,
		RateMax:       10,
	}
}

func newHarness(t *testing.T, trialDays int32) *harness {
	t.Helper()
	return newHarnessOpts(t, defaultHarnessOptions(trialDays))
}

// newHarnessOpts builds the domain with explicit limiter options so tests
// can pin a custom prefix/window.
func newHarnessOpts(t *testing.T, opts Options) *harness {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, dbx.AutoMigrate(db, models.AllModels()...))
	rdb := redisx.NewTestClient(t)
	signer, err := cert.NewSigner(map[string]string{"k1": testSeed}, "")
	require.NoError(t, err)
	return &harness{svc: New(db, rdb, signer, opts), db: db, rdb: rdb}
}

// flush resets the rate-limit window between scenario steps.
func (h *harness) flush(t *testing.T) {
	t.Helper()
	require.NoError(t, h.rdb.FlushAll(context.Background()).Err())
}

// seedKey inserts an active key via dal and returns its normalized form.
func (h *harness) seedKey(t *testing.T, norm string, slots int32) string {
	t.Helper()
	require.NoError(t, dal.CreateKey(context.Background(), h.db, &models.LicenseKey{
		KeyHash:   KeyHash(norm),
		LicenseID: LicenseID(KeyHash(norm)),
		KeyPrefix: norm[:8],
		Label:     "test",
		MaxSlots:  slots,
		Status:    int32(licensev1.KeyStatus_KEY_STATUS_ACTIVE),
	}))
	return norm
}

func (h *harness) seedEntitlement(t *testing.T, norm string, module licensev1.Module, kind licensev1.EntitlementKind, expiresAt *time.Time) {
	t.Helper()
	require.NoError(t, dal.UpsertEntitlement(context.Background(), h.db, &models.LicenseEntitlement{
		KeyHash:   KeyHash(norm),
		Module:    int32(module),
		Kind:      int32(kind),
		ExpiresAt: expiresAt,
		GrantedAt: time.Now(),
	}))
}

func (h *harness) devices(t *testing.T, keyHash string) []*models.LicenseDevice {
	t.Helper()
	rows, err := dal.ListDevicesByKeyHash(context.Background(), h.db, keyHash)
	require.NoError(t, err)
	return rows
}

func actReq(key, fp, token, evict string) *licensev1.ActivateRequest {
	req := &licensev1.ActivateRequest{
		Key:           key,
		FingerprintId: fp,
		DeviceToken:   token,
	}
	if evict != "" {
		req.EvictDeviceToken = &evict
	}
	return req
}

const (
	fp1 = "v1.1111111111111111111111111111111111111111111111111111111111111111"
	fp2 = "v1.2222222222222222222222222222222222222222222222222222222222222222"
	fp3 = "v1.3333333333333333333333333333333333333333333333333333333333333333"
)

func tok(i int) string { return fmt.Sprintf("%08d-0000-4000-8000-%012d", i, i) }

// A real normalized key shape (content arbitrary but shape-valid).
const keyA = "AV1DABCDEFGHIJKLMNOPQRST"

// ─── A1–A12 scenario table (design doc §7.1) ───────────────────────────────

// A1: first activation inserts a row and issues a cert.
func TestA1_FirstActivation(t *testing.T) {
	h := newHarness(t, 14)
	h.seedKey(t, keyA, 3)
	h.seedEntitlement(t, keyA, licensev1.Module_MODULE_TOOLS, licensev1.EntitlementKind_ENTITLEMENT_KIND_PERPETUAL, nil)

	resp, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)
	require.EqualValues(t, 1, resp.GetSlots().GetUsed())
	require.EqualValues(t, 3, resp.GetSlots().GetMax())

	parsed, err := cert.UnmarshalCanonical([]byte(resp.GetPayload()))
	require.NoError(t, err)
	require.Equal(t, LicenseID(KeyHash(keyA)), *parsed.LicenseID)
	require.Equal(t, tok(1), parsed.DeviceToken)
	require.Contains(t, parsed.Entitlements, "tools")
	require.Equal(t, cert.KindPerpetual, parsed.Entitlements["tools"].Kind)
	require.Nil(t, parsed.Entitlements["tools"].ExpiresAt)
}

// A2: heartbeat reuses the slot, refreshes last_seen, re-signs with a fresh
// certId and a non-decreasing issuedAt.
func TestA2_Heartbeat(t *testing.T) {
	h := newHarness(t, 14)
	h.seedKey(t, keyA, 3)

	first, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)
	before := h.devices(t, KeyHash(keyA))[0].LastSeenAt

	h.flush(t)
	second, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)
	require.EqualValues(t, 1, second.GetSlots().GetUsed())

	p1, _ := cert.UnmarshalCanonical([]byte(first.GetPayload()))
	p2, _ := cert.UnmarshalCanonical([]byte(second.GetPayload()))
	require.NotEqual(t, p1.CertID, p2.CertID, "certId must be fresh per issuance")
	require.False(t, p2.IssuedAt.Before(p1.IssuedAt), "issuedAt must never move backwards")

	after := h.devices(t, KeyHash(keyA))[0].LastSeenAt
	require.False(t, after.Before(before))
}

// A3: token-matched fingerprint drift rebinds in place (row count constant,
// last_fingerprint_at stamped).
func TestA3_FingerprintRebind(t *testing.T) {
	h := newHarness(t, 14)
	h.seedKey(t, keyA, 3)

	_, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)

	h.flush(t)
	_, err = h.svc.Activate(context.Background(), actReq(keyA, fp2, tok(1), ""))
	require.NoError(t, err)

	rows := h.devices(t, KeyHash(keyA))
	require.Len(t, rows, 1, "rebind must not consume a second slot")
	require.Equal(t, fp2, rows[0].FingerprintID)
	require.NotNil(t, rows[0].LastFingerprintAt)

	// Repeated drift keeps exactly one row.
	for i, fp := range []string{fp1, fp2, fp3, fp1} {
		h.flush(t)
		_, err = h.svc.Activate(context.Background(), actReq(keyA, fp, tok(1), ""))
		require.NoError(t, err, "drift round %d", i)
	}
	require.Len(t, h.devices(t, KeyHash(keyA)), 1)
}

// A4: post-purchase refresh — new entitlements appear on the next activate
// with no special branch.
func TestA4_EntitlementRefresh(t *testing.T) {
	h := newHarness(t, 14)
	h.seedKey(t, keyA, 3)

	_, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)

	h.seedEntitlement(t, keyA, licensev1.Module_MODULE_DOWNLOADS, licensev1.EntitlementKind_ENTITLEMENT_KIND_PERPETUAL, nil)
	h.flush(t)
	resp, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)

	parsed, _ := cert.UnmarshalCanonical([]byte(resp.GetPayload()))
	require.Contains(t, parsed.Entitlements, "downloads")
}

// A5/A12: slot exhaustion returns the detailed SLOT_LIMIT error with the
// roster attached (also covers reinstall-after-refill).
func TestA5_SlotLimitWithRoster(t *testing.T) {
	h := newHarness(t, 14)
	h.seedKey(t, keyA, 3)
	for i := 1; i <= 3; i++ {
		_, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(i), ""))
		require.NoError(t, err)
		h.flush(t)
	}

	_, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(9), ""))
	require.Error(t, err)
	require.ErrorIs(t, err, xcodes.ErrSlotLimit.New())

	var det *xcodes.Detailed
	require.ErrorAs(t, err, &det)
	found := false
	for _, d := range det.Details {
		if info, ok := d.(*licensev1.SlotLimitInfo); ok {
			found = true
			require.EqualValues(t, 3, info.GetMaxSlots())
			require.Len(t, info.GetDevices(), 3)
		}
	}
	require.True(t, found, "SLOT_LIMIT must carry SlotLimitInfo for the device chooser")
}

// A6: evict-retry succeeds immediately after the 409 — the window quota
// (default 10) absorbs the retry without any refund mechanism.
func TestA6_EvictRetryWithinQuota(t *testing.T) {
	h := newHarness(t, 14)
	h.seedKey(t, keyA, 3)
	for i := 1; i <= 3; i++ {
		_, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(i), ""))
		require.NoError(t, err)
		h.flush(t)
	}

	// 409 first (consumes one quota unit)…
	_, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(9), ""))
	require.ErrorIs(t, err, xcodes.ErrSlotLimit.New())

	// …then the immediate retry with an evict target succeeds — it stays
	// inside the window quota.
	resp, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(9), tok(1)))
	require.NoError(t, err)
	require.EqualValues(t, 3, resp.GetSlots().GetUsed())

	tokens := map[string]bool{}
	for _, d := range h.devices(t, KeyHash(keyA)) {
		tokens[d.DeviceToken] = true
	}
	require.False(t, tokens[tok(1)], "evicted device must be gone")
	require.True(t, tokens[tok(9)])
}

// A7: evicting an already-released target deletes zero rows and continues.
func TestA7_EvictAlreadyReleased(t *testing.T) {
	h := newHarness(t, 14)
	h.seedKey(t, keyA, 3)

	// One slot occupied; evict a token that was never in a slot.
	resp, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), tok(42)))
	require.NoError(t, err)
	require.EqualValues(t, 1, resp.GetSlots().GetUsed())
}

// Edge: evicting your own (not-yet-inserted) token equals no evict — still
// 409 when full.
func TestEvictOwnTokenIsNoEvict(t *testing.T) {
	h := newHarness(t, 14)
	h.seedKey(t, keyA, 1)
	_, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)

	h.flush(t)
	_, err = h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(2), tok(2)))
	require.ErrorIs(t, err, xcodes.ErrSlotLimit.New())
}

// Edge: max_slots=1 full lifecycle rotation.
func TestMaxSlotsOneRotation(t *testing.T) {
	h := newHarness(t, 14)
	h.seedKey(t, keyA, 1)

	_, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)

	h.flush(t)
	_, err = h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(2), ""))
	require.ErrorIs(t, err, xcodes.ErrSlotLimit.New())

	h.flush(t)
	_, err = h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(2), tok(1)))
	require.NoError(t, err)
	require.Len(t, h.devices(t, KeyHash(keyA)), 1)
}

// A8: an expired subscription stays in the cert (client judges expiry).
func TestA8_ExpiredEntitlementStillIssued(t *testing.T) {
	h := newHarness(t, 14)
	h.seedKey(t, keyA, 3)
	past := time.Now().Add(-24 * time.Hour)
	h.seedEntitlement(t, keyA, licensev1.Module_MODULE_TOOLS, licensev1.EntitlementKind_ENTITLEMENT_KIND_SUBSCRIPTION, &past)

	resp, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)
	parsed, _ := cert.UnmarshalCanonical([]byte(resp.GetPayload()))
	require.Contains(t, parsed.Entitlements, "tools")
	require.NotNil(t, parsed.Entitlements["tools"].ExpiresAt)
	require.True(t, parsed.Entitlements["tools"].ExpiresAt.Before(time.Now()))
}

// A9: revoked keys reject everyone, including devices already in a slot.
func TestA9_RevokedKey(t *testing.T) {
	h := newHarness(t, 14)
	h.seedKey(t, keyA, 3)
	_, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)

	now := time.Now()
	require.NoError(t, dal.SetKeyStatus(context.Background(), h.db, KeyHash(keyA),
		int32(licensev1.KeyStatus_KEY_STATUS_REVOKED), &now))

	h.flush(t)
	_, err = h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), ""))
	require.ErrorIs(t, err, xcodes.ErrKeyRevoked.New(), "in-slot heartbeat must be rejected too")

	// Deactivate stays allowed (cleanup is harmless).
	h.flush(t)
	deact, err := h.svc.Deactivate(context.Background(), &licensev1.DeactivateRequest{Key: keyA, DeviceToken: tok(1)})
	require.NoError(t, err)
	require.True(t, deact.GetReleased())
}

// A10: unknown key.
func TestA10_KeyNotFound(t *testing.T) {
	h := newHarness(t, 14)
	_, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), ""))
	require.ErrorIs(t, err, xcodes.ErrKeyNotFound.New())
}

// A11: rate limit — exhausting the per-window quota is denied with a
// RetryAfterInfo detail (harness pins Max=1 so the second call exhausts it).
func TestA11_RateLimited(t *testing.T) {
	opts := defaultHarnessOptions(14)
	opts.RateMax = 1 // pin quota 1 so the second call exhausts it
	h := newHarnessOpts(t, opts)
	h.seedKey(t, keyA, 3)

	_, err := h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)

	_, err = h.svc.Activate(context.Background(), actReq(keyA, fp1, tok(1), ""))
	require.ErrorIs(t, err, xcodes.ErrRateLimited.New())
	var det *xcodes.Detailed
	require.ErrorAs(t, err, &det)
	found := false
	for _, d := range det.Details {
		if info, ok := d.(*licensev1.RetryAfterInfo); ok {
			found = true
			require.EqualValues(t, 60, info.GetSeconds())
		}
	}
	require.True(t, found, "RATE_LIMITED must carry RetryAfterInfo")
}

// Key normalization errors: dual-empty and malformed shapes.
func TestBadKeyFormat(t *testing.T) {
	h := newHarness(t, 14)
	ctx := context.Background()

	_, err := h.svc.Activate(ctx, actReq("", fp1, tok(1), ""))
	require.ErrorIs(t, err, xcodes.ErrBadKeyFormat.New())
	_, err = h.svc.Activate(ctx, &licensev1.ActivateRequest{FingerprintId: fp1, DeviceToken: tok(1)})
	require.ErrorIs(t, err, xcodes.ErrBadKeyFormat.New())
	_, err = h.svc.Activate(ctx, actReq("WRONG-FORMAT-KEY", fp1, tok(1), ""))
	require.ErrorIs(t, err, xcodes.ErrBadKeyFormat.New())

	// The license_key alias is accepted (docs wrote licenseKey; the client
	// sends key — both work).
	h.seedKey(t, keyA, 3)
	_, err = h.svc.Activate(ctx, &licensev1.ActivateRequest{
		LicenseKey:    keyA,
		FingerprintId: fp1,
		DeviceToken:   tok(1),
	})
	require.NoError(t, err)

	// Normalization accepts lowercase and dashed input.
	_, err = h.svc.Activate(ctx, actReq("av1d-abcde-fghjk-mnpqr-stuvw", fp1, tok(2), ""))
	require.ErrorIs(t, err, xcodes.ErrKeyNotFound.New(), "normalized ok, just unknown")
}

// Deactivate idempotency: never-in-slot and unknown-key both return 200
// released=false.
func TestDeactivateIdempotent(t *testing.T) {
	h := newHarness(t, 14)
	h.seedKey(t, keyA, 3)
	ctx := context.Background()

	resp, err := h.svc.Deactivate(ctx, &licensev1.DeactivateRequest{Key: keyA, DeviceToken: tok(1)})
	require.NoError(t, err)
	require.False(t, resp.GetReleased())

	resp, err = h.svc.Deactivate(ctx, &licensev1.DeactivateRequest{Key: "AV1DZZZZZZZZZZZZZZZZZZZZ", DeviceToken: tok(1)})
	require.NoError(t, err)
	require.False(t, resp.GetReleased())

	h.flush(t)
	_, err = h.svc.Activate(ctx, actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)
	h.flush(t)
	resp, err = h.svc.Deactivate(ctx, &licensev1.DeactivateRequest{Key: keyA, DeviceToken: tok(1)})
	require.NoError(t, err)
	require.True(t, resp.GetReleased())
	require.Empty(t, h.devices(t, KeyHash(keyA)))
}

// Concurrent last-slot race: two fresh tokens race one remaining slot,
// exactly one wins (FOR UPDATE serialization).
func TestConcurrentLastSlot(t *testing.T) {
	h := newHarness(t, 14)
	h.seedKey(t, keyA, 2)
	ctx := context.Background()

	_, err := h.svc.Activate(ctx, actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)
	h.flush(t)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = h.svc.Activate(ctx, actReq(keyA, fp1, tok(10+i), ""))
		}(i)
	}
	wg.Wait()

	var ok, limited int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, xcodes.ErrSlotLimit.New()):
			limited++
		default:
			t.Fatalf("unexpected racer error: %v", err)
		}
	}
	require.Equal(t, 1, ok, "exactly one racer takes the last slot")
	require.Equal(t, 1, limited)
	require.Len(t, h.devices(t, KeyHash(keyA)), 2)
}

// ─── trial/start (design doc §7.4) ──────────────────────────────────────────

func trialReq(module, fp, token, key string) *licensev1.TrialStartRequest {
	return &licensev1.TrialStartRequest{
		Module:        module,
		FingerprintId: fp,
		DeviceToken:   token,
		LicenseKey:    key,
	}
}

// Keyless start: licenseId null, trial expiry derived server-side; restart
// with a fresh token (reinstall) is alreadyStarted and keeps started_at.
func TestTrialKeylessStartAndRestart(t *testing.T) {
	h := newHarness(t, 14)
	ctx := context.Background()

	resp, err := h.svc.TrialStart(ctx, trialReq("downloads", fp1, tok(1), ""))
	require.NoError(t, err)
	require.False(t, resp.GetAlreadyStarted())

	parsed, _ := cert.UnmarshalCanonical([]byte(resp.GetPayload()))
	require.Nil(t, parsed.LicenseID, "keyless cert has licenseId null")
	require.Contains(t, parsed.Entitlements, "downloads")
	require.Equal(t, cert.KindTrial, parsed.Entitlements["downloads"].Kind)
	require.NotNil(t, parsed.Entitlements["downloads"].ExpiresAt)

	first, err := dal.GetTrial(ctx, h.db, fp1, int32(licensev1.Module_MODULE_DOWNLOADS))
	require.NoError(t, err)

	h.flush(t)
	resp, err = h.svc.TrialStart(ctx, trialReq("downloads", fp1, tok(2), ""))
	require.NoError(t, err)
	require.True(t, resp.GetAlreadyStarted(), "reinstall with a new token must not reset")

	second, err := dal.GetTrial(ctx, h.db, fp1, int32(licensev1.Module_MODULE_DOWNLOADS))
	require.NoError(t, err)
	require.Equal(t, first.StartedAt, second.StartedAt)
}

// Trial days boundary: trialDays=1 derives expiry = started_at + 1d.
func TestTrialExpiryFromDays(t *testing.T) {
	h := newHarness(t, 1)

	resp, err := h.svc.TrialStart(context.Background(), trialReq("tools", fp1, tok(1), ""))
	require.NoError(t, err)
	parsed, _ := cert.UnmarshalCanonical([]byte(resp.GetPayload()))
	require.WithinDuration(t,
		parsed.IssuedAt.Add(24*time.Hour),
		*parsed.Entitlements["tools"].ExpiresAt,
		2*time.Second)
}

// Concurrent starts collapse to one ledger row; N-1 report alreadyStarted.
func TestTrialConcurrentStart(t *testing.T) {
	h := newHarness(t, 14)
	ctx := context.Background()
	const workers = 8

	var wg sync.WaitGroup
	resps := make([]*licensev1.TrialStartResponse, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resps[i], _ = h.svc.TrialStart(ctx, trialReq("downloads", fp1, tok(i), ""))
		}(i)
	}
	wg.Wait()

	var already int
	for _, r := range resps {
		if r != nil && r.GetAlreadyStarted() {
			already++
		}
	}
	require.Equal(t, workers-1, already)
	rows, err := dal.ListTrialsByFingerprint(ctx, h.db, fp1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

// Keyless certs carry the FULL ledger, expired entries included (the client
// derives trialUsed from it).
func TestTrialKeylessLedgerIncludesExpired(t *testing.T) {
	h := newHarness(t, 14)
	ctx := context.Background()

	_, err := h.svc.TrialStart(ctx, trialReq("downloads", fp1, tok(1), ""))
	require.NoError(t, err)

	// Backdate the tools ledger entry beyond expiry.
	old := time.Now().AddDate(0, 0, -30)
	require.NoError(t, dal.InsertTrialOnConflictDoNothing(ctx, h.db, &models.LicenseTrial{
		FingerprintID:    fp1,
		Module:           int32(licensev1.Module_MODULE_TOOLS),
		StartedAt:        old,
		FirstDeviceToken: tok(9),
	}))

	h.flush(t)
	resp, err := h.svc.TrialStart(ctx, trialReq("downloads", fp1, tok(1), ""))
	require.NoError(t, err)
	parsed, _ := cert.UnmarshalCanonical([]byte(resp.GetPayload()))
	require.Contains(t, parsed.Entitlements, "tools", "expired trial entries stay in the keyless cert")
	require.True(t, parsed.Entitlements["tools"].ExpiresAt.Before(time.Now()))
}

// Keyed merge: key entitlements ∪ ledger with key priority on collisions.
func TestTrialKeyedMerge(t *testing.T) {
	h := newHarness(t, 14)
	ctx := context.Background()
	h.seedKey(t, keyA, 3)
	h.seedEntitlement(t, keyA, licensev1.Module_MODULE_DOWNLOADS, licensev1.EntitlementKind_ENTITLEMENT_KIND_PERPETUAL, nil)

	// tools trial already running keyless.
	_, err := h.svc.TrialStart(ctx, trialReq("tools", fp1, tok(1), ""))
	require.NoError(t, err)

	// Keyed trial start for tools (not entitled on the key): merges.
	h.flush(t)
	resp, err := h.svc.TrialStart(ctx, trialReq("tools", fp1, tok(2), keyA))
	require.NoError(t, err)
	parsed, _ := cert.UnmarshalCanonical([]byte(resp.GetPayload()))
	require.Equal(t, LicenseID(KeyHash(keyA)), *parsed.LicenseID)
	require.Contains(t, parsed.Entitlements, "downloads")
	require.Equal(t, cert.KindPerpetual, parsed.Entitlements["downloads"].Kind)
	require.Contains(t, parsed.Entitlements, "tools")
	require.Equal(t, cert.KindTrial, parsed.Entitlements["tools"].Kind)
	require.Len(t, h.devices(t, KeyHash(keyA)), 1, "keyed trial start occupies one key slot; the keyless trial took none")
}

// Keyed with a still-valid entitlement for the module → ALREADY_ENTITLED;
// an expired one does not block.
func TestTrialAlreadyEntitled(t *testing.T) {
	h := newHarness(t, 14)
	ctx := context.Background()
	h.seedKey(t, keyA, 3)
	future := time.Now().Add(24 * time.Hour)
	h.seedEntitlement(t, keyA, licensev1.Module_MODULE_TOOLS, licensev1.EntitlementKind_ENTITLEMENT_KIND_SUBSCRIPTION, &future)

	_, err := h.svc.TrialStart(ctx, trialReq("tools", fp1, tok(1), keyA))
	require.ErrorIs(t, err, xcodes.ErrAlreadyEntitled.New())

	past := time.Now().Add(-24 * time.Hour)
	h.seedEntitlement(t, keyA, licensev1.Module_MODULE_TOOLS, licensev1.EntitlementKind_ENTITLEMENT_KIND_SUBSCRIPTION, &past)
	h.flush(t)
	_, err = h.svc.TrialStart(ctx, trialReq("tools", fp1, tok(1), keyA))
	require.NoError(t, err, "expired entitlement must not block a trial")
}

// Keyed trial start on a full key runs the same slot discipline (409).
func TestTrialKeyedSlotDiscipline(t *testing.T) {
	h := newHarness(t, 14)
	ctx := context.Background()
	h.seedKey(t, keyA, 1)

	_, err := h.svc.Activate(ctx, actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)

	h.flush(t)
	_, err = h.svc.TrialStart(ctx, trialReq("tools", fp1, tok(2), keyA))
	require.ErrorIs(t, err, xcodes.ErrSlotLimit.New())
}

// ─── key material unit checks ───────────────────────────────────────────────

func TestKeyMaterial(t *testing.T) {
	norm, err := NormalizeKey("  av1d-abcde-fghjk-mnpqr-stuvw  ")
	require.NoError(t, err)
	require.Regexp(t, `^AV1D[0-9A-Z]{20}$`, norm)

	plaintext, err := GenerateKey()
	require.NoError(t, err)
	require.Regexp(t, `^AV1D(-[0-9A-Z]{5}){4}$`, plaintext)
	gennorm, err := NormalizeKey(plaintext)
	require.NoError(t, err)
	require.Equal(t, strings.ReplaceAll(plaintext, "-", ""), gennorm)

	hash := KeyHash(gennorm)
	require.Len(t, hash, 64)
	require.Equal(t, "lk_"+hash[:32], LicenseID(hash))
}

// Rate-limit configuration takes effect: custom key prefix lands in Redis,
// the quota and window drive denial, TTL and Retry-After derive from the
// window. Keys follow go-common ratelimit's shape
// <prefix>:<purpose>:{<target>}:<window_seconds>.
func TestRateLimitConfigurable(t *testing.T) {
	h := newHarnessOpts(t, Options{
		TrialDays:     14,
		RateKeyPrefix: "lic:rl",
		RateWindow:    time.Second,
		RateMax:       2,
	})
	ctx := context.Background()
	h.seedKey(t, keyA, 3)

	_, err := h.svc.Activate(ctx, actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err)
	_, err = h.svc.Activate(ctx, actReq(keyA, fp1, tok(1), ""))
	require.NoError(t, err, "second request stays inside the Max=2 quota")

	// Custom prefix visible on the redis key; default prefix unused.
	keys := h.rdb.Keys(ctx, "lic:rl:*").Val()
	require.Len(t, keys, 1, "limiter key must use the configured prefix")
	require.Contains(t, keys[0], "activate:{", "go-common ratelimit key shape")
	require.Empty(t, h.rdb.Keys(ctx, "license:rate:*").Val(), "default prefix must not be used")

	// Third hit exhausts the quota; the hint derives from the window.
	_, err = h.svc.Activate(ctx, actReq(keyA, fp1, tok(1), ""))
	require.ErrorIs(t, err, xcodes.ErrRateLimited.New())
	var det *xcodes.Detailed
	require.ErrorAs(t, err, &det)
	hinted := false
	for _, d := range det.Details {
		if info, ok := d.(*licensev1.RetryAfterInfo); ok {
			require.EqualValues(t, 1, info.GetSeconds(), "Retry-After derives from the configured window")
			hinted = true
		}
	}
	require.True(t, hinted)

	// The window lands on the key TTL (real Redis expires it after the
	// configured duration; miniredis's clock only moves via FastForward, so
	// the TTL value is asserted instead of a wall-clock sleep).
	ttl := h.rdb.TTL(ctx, keys[0]).Val()
	require.Equal(t, time.Second, ttl, "key TTL must equal the configured window")

	// A different device_token is a different quota bucket.
	_, err = h.svc.Activate(ctx, actReq(keyA, fp1, tok(2), ""))
	require.NoError(t, err, "quota is per (identity, device_token)")
}
