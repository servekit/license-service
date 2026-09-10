package admin_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"

	"github.com/stretchr/testify/require"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/redisx"

	licensev1 "github.com/servekit/api/gen/go/license/v1"
	"github.com/servekit/license-service/internal/service/activation"
	"github.com/servekit/license-service/internal/service/admin"
	"github.com/servekit/license-service/internal/service/cert"
	"github.com/servekit/license-service/internal/store/models"
	"github.com/servekit/license-service/pkg/xcodes"
)

const (
	testSeed = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
	fp1      = "v1.4444444444444444444444444444444444444444444444444444444444444444"
)

func tok(i int) string {
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i)
}

type harness struct {
	admin *admin.Service
	act   *activation.Service
	db    *gorm.DB
	rdb   *redis.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, dbx.AutoMigrate(db, models.AllModels()...))
	rdb := redisx.NewTestClient(t)
	signer, err := cert.NewSigner(map[string]string{"k1": testSeed}, "")
	require.NoError(t, err)
	return &harness{
		admin: admin.New(db, signer, 14),
		act: activation.New(db, rdb, signer, activation.Options{
			TrialDays:     14,
			RateKeyPrefix: "license:rate",
			RateWindow:    time.Minute,
			RateMax:       10,
		}),
		db:  db,
		rdb: rdb,
	}
}

func (h *harness) flush(t *testing.T) {
	t.Helper()
	require.NoError(t, h.rdb.FlushAll(context.Background()).Err())
}

// TestCreateKeyLoopWithActivate: the full operator flow — mint a key with
// grants, activate with the returned plaintext, see the entitlements in the
// signed cert.

func TestCreateKeyLoopWithActivate(t *testing.T) {
	h := newHarness(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity

	created, err := h.admin.CreateKey(ctx, &licensev1.CreateKeyRequest{
		Label: "order-42",
		Slots: 5,
		Grants: []*licensev1.EntitlementInput{
			{Module: licensev1.Module_MODULE_DOWNLOADS, Kind: licensev1.EntitlementKind_ENTITLEMENT_KIND_PERPETUAL},
			{Module: licensev1.Module_MODULE_TOOLS, Kind: licensev1.EntitlementKind_ENTITLEMENT_KIND_SUBSCRIPTION, DurationDays: 365},
		},
	})
	require.NoError(t, err)
	require.Regexp(t, `^AV1D(-[0-9A-Z]{5}){4}$`, created.GetPlaintextKey())
	require.Equal(t, "order-42", created.GetKey().GetLabel())
	require.EqualValues(t, 5, created.GetKey().GetMaxSlots())
	require.EqualValues(t, 2, len(created.GetKey().GetEntitlements()))

	// Slots default.
	def, err := h.admin.CreateKey(ctx, &licensev1.CreateKeyRequest{})
	require.NoError(t, err)
	require.EqualValues(t, 3, def.GetKey().GetMaxSlots())

	// Activate with the returned plaintext.
	resp, err := h.act.Activate(ctx, &licensev1.ActivateRequest{
		Key:           created.GetPlaintextKey(),
		FingerprintId: fp1,
		DeviceToken:   tok(1),
	})
	require.NoError(t, err)
	parsed, err := cert.UnmarshalCanonical([]byte(resp.GetPayload()))
	require.NoError(t, err)
	require.Contains(t, parsed.Entitlements, "downloads")
	require.Equal(t, cert.KindPerpetual, parsed.Entitlements["downloads"].Kind)
	require.Contains(t, parsed.Entitlements, "tools")
	require.NotNil(t, parsed.Entitlements["tools"].ExpiresAt)
	require.WithinDuration(t, time.Now().AddDate(0, 0, 365), *parsed.Entitlements["tools"].ExpiresAt, time.Minute)

	// ShowKey by both id forms.
	byID, err := h.admin.ShowKey(ctx, &licensev1.ShowKeyRequest{KeyId: created.GetKey().GetLicenseId()})
	require.NoError(t, err)
	require.EqualValues(t, 1, byID.GetKey().GetUsedSlots())
	require.Len(t, byID.GetKey().GetDevices(), 1)
	require.Regexp(t, `^AV1D`, byID.GetKey().GetKeyPrefix())

	var row *models.LicenseKey
	require.NoError(t, h.db.Where("license_id = ?", created.GetKey().GetLicenseId()).Take(&row).Error)
	byHash, err := h.admin.ShowKey(ctx, &licensev1.ShowKeyRequest{KeyId: row.KeyHash})
	require.NoError(t, err)
	require.Equal(t, byID.GetKey().GetLicenseId(), byHash.GetKey().GetLicenseId())
}

// TestCreateKeyValidation: perpetual grants must not carry expiry; the
// duration/exclusive rule is enforced server-side too (CEL guards the wire).
func TestCreateKeyValidation(t *testing.T) {
	h := newHarness(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity

	_, err := h.admin.CreateKey(ctx, &licensev1.CreateKeyRequest{
		Grants: []*licensev1.EntitlementInput{{
			Module: licensev1.Module_MODULE_TOOLS,
			Kind:   licensev1.EntitlementKind_ENTITLEMENT_KIND_PERPETUAL,
			//nolint:gosec // test fixture
			DurationDays: 30,
		}},
	})
	require.ErrorIs(t, err, xcodes.ErrBadRequest.New())

	_, err = h.admin.CreateKey(ctx, &licensev1.CreateKeyRequest{
		Grants: []*licensev1.EntitlementInput{{
			Module: licensev1.Module_MODULE_TOOLS,
			Kind:   licensev1.EntitlementKind_ENTITLEMENT_KIND_SUBSCRIPTION,
		}},
	})
	require.ErrorIs(t, err, xcodes.ErrBadRequest.New(), "subscription needs an expiry source")
}

// TestRevokeUnrevokeLifecycle: revoke rejects activation (including in-slot
// heartbeats), unrevoke reattaches slots as they were.
func TestRevokeUnrevokeLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity
	created, err := h.admin.CreateKey(ctx, &licensev1.CreateKeyRequest{})
	require.NoError(t, err)
	licenseID := created.GetKey().GetLicenseId()

	_, err = h.act.Activate(ctx, &licensev1.ActivateRequest{Key: created.GetPlaintextKey(), FingerprintId: fp1, DeviceToken: tok(1)})
	require.NoError(t, err)

	revoked, err := h.admin.RevokeKey(ctx, &licensev1.RevokeKeyRequest{KeyId: licenseID, Reason: ptr("chargeback")})
	require.NoError(t, err)
	require.Equal(t, licensev1.KeyStatus_KEY_STATUS_REVOKED, revoked.GetKey().GetStatus())
	require.NotNil(t, revoked.GetKey().GetRevokedAt())

	h.flush(t)
	_, err = h.act.Activate(ctx, &licensev1.ActivateRequest{Key: created.GetPlaintextKey(), FingerprintId: fp1, DeviceToken: tok(1)})
	require.ErrorIs(t, err, xcodes.ErrKeyRevoked.New())

	unrevoked, err := h.admin.UnrevokeKey(ctx, &licensev1.UnrevokeKeyRequest{KeyId: licenseID})
	require.NoError(t, err)
	require.Equal(t, licensev1.KeyStatus_KEY_STATUS_ACTIVE, unrevoked.GetKey().GetStatus())

	h.flush(t)
	resp, err := h.act.Activate(ctx, &licensev1.ActivateRequest{Key: created.GetPlaintextKey(), FingerprintId: fp1, DeviceToken: tok(1)})
	require.NoError(t, err)
	require.EqualValues(t, 1, resp.GetSlots().GetUsed(), "slots reattach after unrevoke")
}

// TestUpdateKey: optional label/slots; empty update rejected.
func TestUpdateKey(t *testing.T) {
	h := newHarness(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity
	created, err := h.admin.CreateKey(ctx, &licensev1.CreateKeyRequest{Label: "before"})
	require.NoError(t, err)
	id := created.GetKey().GetLicenseId()

	_, err = h.admin.UpdateKey(ctx, &licensev1.UpdateKeyRequest{KeyId: id})
	require.ErrorIs(t, err, xcodes.ErrBadRequest.New())

	updated, err := h.admin.UpdateKey(ctx, &licensev1.UpdateKeyRequest{KeyId: id, Label: ptr("after"), Slots: ptr(int32(7))})
	require.NoError(t, err)
	require.Equal(t, "after", updated.GetKey().GetLabel())
	require.EqualValues(t, 7, updated.GetKey().GetMaxSlots())
}

// TestDeleteKeyCascade: physical delete removes keys, entitlements and
// devices together; confirm is mandatory.
func TestDeleteKeyCascade(t *testing.T) {
	h := newHarness(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity
	created, err := h.admin.CreateKey(ctx, &licensev1.CreateKeyRequest{
		Grants: []*licensev1.EntitlementInput{
			{Module: licensev1.Module_MODULE_TOOLS, Kind: licensev1.EntitlementKind_ENTITLEMENT_KIND_PERPETUAL},
		},
	})
	require.NoError(t, err)
	_, err = h.act.Activate(ctx, &licensev1.ActivateRequest{Key: created.GetPlaintextKey(), FingerprintId: fp1, DeviceToken: tok(1)})
	require.NoError(t, err)

	var row models.LicenseKey
	require.NoError(t, h.db.Where("license_id = ?", created.GetKey().GetLicenseId()).Take(&row).Error)

	_, err = h.admin.DeleteKey(ctx, &licensev1.DeleteKeyRequest{KeyId: created.GetKey().GetLicenseId()})
	require.ErrorIs(t, err, xcodes.ErrBadRequest.New(), "confirm is mandatory")

	_, err = h.admin.DeleteKey(ctx, &licensev1.DeleteKeyRequest{KeyId: created.GetKey().GetLicenseId(), Confirm: ptr(true)})
	require.NoError(t, err)

	var count int64
	require.NoError(t, h.db.Model(&models.LicenseKey{}).Where("key_hash = ?", row.KeyHash).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, h.db.Model(&models.LicenseEntitlement{}).Where("key_hash = ?", row.KeyHash).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, h.db.Model(&models.LicenseDevice{}).Where("key_hash = ?", row.KeyHash).Count(&count).Error)
	require.Zero(t, count)
}

// TestGrantModuleExtensionRules: duration extends from max(now, current
// expiry); absolute expiry overrides; perpetual+expiry rejected.
func TestGrantModuleExtensionRules(t *testing.T) {
	h := newHarness(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity
	created, err := h.admin.CreateKey(ctx, &licensev1.CreateKeyRequest{})
	require.NoError(t, err)
	id := created.GetKey().GetLicenseId()

	// First grant: 30d from now.
	g1, err := h.admin.GrantModule(ctx, &licensev1.GrantModuleRequest{
		KeyId: id, Module: licensev1.Module_MODULE_TOOLS,
		Kind: licensev1.EntitlementKind_ENTITLEMENT_KIND_SUBSCRIPTION, DurationDays: 30,
	})
	require.NoError(t, err)
	first := g1.GetEntitlement().GetExpiresAt().AsTime()

	// Re-grant 30d BEFORE expiry: extends from the unexpired remainder
	// (base = first), not from now.
	g2, err := h.admin.GrantModule(ctx, &licensev1.GrantModuleRequest{
		KeyId: id, Module: licensev1.Module_MODULE_TOOLS,
		Kind: licensev1.EntitlementKind_ENTITLEMENT_KIND_SUBSCRIPTION, DurationDays: 30,
	})
	require.NoError(t, err)
	extended := g2.GetEntitlement().GetExpiresAt().AsTime()
	require.WithinDuration(t, first.AddDate(0, 0, 30), extended, time.Minute)

	// Absolute override.
	abs := time.Now().Add(72 * time.Hour).UTC()
	g3, err := h.admin.GrantModule(ctx, &licensev1.GrantModuleRequest{
		KeyId: id, Module: licensev1.Module_MODULE_TOOLS,
		Kind: licensev1.EntitlementKind_ENTITLEMENT_KIND_SUBSCRIPTION, ExpiresAt: timestamppb.New(abs),
	})
	require.NoError(t, err)
	require.WithinDuration(t, abs, g3.GetEntitlement().GetExpiresAt().AsTime(), time.Second)

	// Perpetual with expiry rejected.
	_, err = h.admin.GrantModule(ctx, &licensev1.GrantModuleRequest{
		KeyId: id, Module: licensev1.Module_MODULE_TOOLS,
		Kind: licensev1.EntitlementKind_ENTITLEMENT_KIND_PERPETUAL, DurationDays: 10,
	})
	require.ErrorIs(t, err, xcodes.ErrBadRequest.New())
}

// TestRevokeModuleDropsFromNextCert.
func TestRevokeModuleDropsFromNextCert(t *testing.T) {
	h := newHarness(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity
	created, err := h.admin.CreateKey(ctx, &licensev1.CreateKeyRequest{
		Grants: []*licensev1.EntitlementInput{
			{Module: licensev1.Module_MODULE_TOOLS, Kind: licensev1.EntitlementKind_ENTITLEMENT_KIND_PERPETUAL},
		},
	})
	require.NoError(t, err)

	resp, err := h.act.Activate(ctx, &licensev1.ActivateRequest{Key: created.GetPlaintextKey(), FingerprintId: fp1, DeviceToken: tok(1)})
	require.NoError(t, err)
	parsed, _ := cert.UnmarshalCanonical([]byte(resp.GetPayload()))
	require.Contains(t, parsed.Entitlements, "tools")

	_, err = h.admin.RevokeModule(ctx, &licensev1.RevokeModuleRequest{KeyId: created.GetKey().GetLicenseId(), Module: licensev1.Module_MODULE_TOOLS})
	require.NoError(t, err)

	h.flush(t)
	resp, err = h.act.Activate(ctx, &licensev1.ActivateRequest{Key: created.GetPlaintextKey(), FingerprintId: fp1, DeviceToken: tok(1)})
	require.NoError(t, err)
	parsed, _ = cert.UnmarshalCanonical([]byte(resp.GetPayload()))
	require.NotContains(t, parsed.Entitlements, "tools")
}

// TestListAndKickDevices.
func TestListAndKickDevices(t *testing.T) {
	h := newHarness(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity
	created, err := h.admin.CreateKey(ctx, &licensev1.CreateKeyRequest{})
	require.NoError(t, err)
	id := created.GetKey().GetLicenseId()

	for i := 1; i <= 2; i++ {
		_, err = h.act.Activate(ctx, &licensev1.ActivateRequest{Key: created.GetPlaintextKey(), FingerprintId: fp1, DeviceToken: tok(i)})
		require.NoError(t, err)
		h.flush(t)
	}

	listed, err := h.admin.ListKeyDevices(ctx, &licensev1.ListKeyDevicesRequest{KeyId: id})
	require.NoError(t, err)
	require.Len(t, listed.GetDevices(), 2)

	kicked, err := h.admin.KickDevice(ctx, &licensev1.KickDeviceRequest{KeyId: id, DeviceToken: listed.GetDevices()[0].GetDeviceToken()})
	require.NoError(t, err)
	require.True(t, kicked.GetKicked())

	again, err := h.admin.KickDevice(ctx, &licensev1.KickDeviceRequest{KeyId: id, DeviceToken: listed.GetDevices()[0].GetDeviceToken()})
	require.NoError(t, err)
	require.False(t, again.GetKicked(), "kicking a released device is a no-op")
}

// TestTrialsShowAndReset: derived expiry, module filter, reset removes the
// row (the only manual channel).
func TestTrialsShowAndReset(t *testing.T) {
	h := newHarness(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity

	_, err := h.act.TrialStart(ctx, &licensev1.TrialStartRequest{Module: "downloads", FingerprintId: fp1, DeviceToken: tok(1)})
	require.NoError(t, err)
	_, err = h.act.TrialStart(ctx, &licensev1.TrialStartRequest{Module: "tools", FingerprintId: fp1, DeviceToken: tok(2)})
	require.NoError(t, err)

	all, err := h.admin.ShowTrial(ctx, &licensev1.ShowTrialRequest{FingerprintId: fp1})
	require.NoError(t, err)
	require.Len(t, all.GetTrials(), 2)

	only, err := h.admin.ShowTrial(ctx, &licensev1.ShowTrialRequest{FingerprintId: fp1, Module: licensev1.Module_MODULE_TOOLS})
	require.NoError(t, err)
	require.Len(t, only.GetTrials(), 1)
	require.WithinDuration(t,
		only.GetTrials()[0].GetStartedAt().AsTime().AddDate(0, 0, 14),
		only.GetTrials()[0].GetExpiresAt().AsTime(),
		time.Second)

	reset, err := h.admin.ResetTrial(ctx, &licensev1.ResetTrialRequest{FingerprintId: fp1, Module: licensev1.Module_MODULE_TOOLS, Reason: "support ticket #7"})
	require.NoError(t, err)
	require.True(t, reset.GetReset_())

	resetAgain, err := h.admin.ResetTrial(ctx, &licensev1.ResetTrialRequest{FingerprintId: fp1, Module: licensev1.Module_MODULE_TOOLS, Reason: "again"})
	require.NoError(t, err)
	require.False(t, resetAgain.GetReset_())

	// The reset fingerprint can start a fresh trial.
	h.flush(t)
	resp, err := h.act.TrialStart(ctx, &licensev1.TrialStartRequest{Module: "tools", FingerprintId: fp1, DeviceToken: tok(3)})
	require.NoError(t, err)
	require.False(t, resp.GetAlreadyStarted())
}

// TestShowPubKey: lists every configured key (sorted) plus the active kid.
func TestShowPubKey(t *testing.T) {
	h := newHarness(t)
	resp, err := h.admin.ShowPubKey(platformCtx(), &licensev1.ShowPubKeyRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetKeys(), 1)
	require.Equal(t, "k1", resp.GetActiveKeyId())
	require.Equal(t, "k1", resp.GetKeys()[0].GetKeyId())
	require.Equal(t, "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=", resp.GetKeys()[0].GetPublicKeyB64())
}

func TestShowPubKeyMultipleKeys(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, dbx.AutoMigrate(db, models.AllModels()...))
	otherSeed := "4ccd089b28ff96da9db6c346ec114e0f5b8058f5e8ad5b7e2b4b1e7c5d3f5a6b"
	signer, err := cert.NewSigner(map[string]string{
		"a-key": testSeed,
		"b-key": otherSeed,
	}, "b-key")
	require.NoError(t, err)
	svc := admin.New(db, signer, 14)

	resp, err := svc.ShowPubKey(platformCtx(), &licensev1.ShowPubKeyRequest{})
	require.NoError(t, err)
	require.Equal(t, "b-key", resp.GetActiveKeyId())
	require.Len(t, resp.GetKeys(), 2)
	require.Equal(t, "a-key", resp.GetKeys()[0].GetKeyId())
	require.Equal(t, "b-key", resp.GetKeys()[1].GetKeyId())
	require.Equal(t, "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=", resp.GetKeys()[0].GetPublicKeyB64())
	require.NotEqual(t, resp.GetKeys()[0].GetPublicKeyB64(), resp.GetKeys()[1].GetPublicKeyB64())
}

// TestListKeys.
func TestListKeys(t *testing.T) {
	h := newHarness(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity
	for i := 0; i < 3; i++ {
		_, err := h.admin.CreateKey(ctx, &licensev1.CreateKeyRequest{Label: "k"})
		require.NoError(t, err)
	}
	all, err := h.admin.ListKeys(ctx, &licensev1.ListKeysRequest{Limit: 10})
	require.NoError(t, err)
	require.Len(t, all.GetKeys(), 3)
}

// Unknown key ids surface KEY_NOT_FOUND across the admin surface.
func TestUnknownKeyID(t *testing.T) {
	h := newHarness(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity
	for _, call := range []func() error{
		func() error {
			_, err := h.admin.ShowKey(ctx, &licensev1.ShowKeyRequest{KeyId: "lk_ffffffffffffffffffffffffffffffff"})
			return err
		},
		func() error {
			_, err := h.admin.RevokeKey(ctx, &licensev1.RevokeKeyRequest{KeyId: "deadbeef"})
			return err
		},
	} {
		require.ErrorIs(t, call(), xcodes.ErrKeyNotFound.New())
	}
}

func ptr[T any](v T) *T { return &v }
