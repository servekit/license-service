// Package integration exercises the full gRPC stack — interceptors, admin
// auth, error-to-status mapping with details, and the wire payload contract
// — against a real server with Postgres (testcontainer) and Redis
// (miniredis). This is the gRPC-side mirror of the design doc's §12.4 wire
// contract tests; the HTTP half belongs to the future standalone gateway.
package integration

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"

	"github.com/stretchr/testify/require"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/redisx"

	licensev1 "github.com/servekit/api/gen/go/license/v1"
	"github.com/servekit/license-service/internal/service/cert"
	"github.com/servekit/license-service/internal/store/models"
	"github.com/servekit/license-service/pkg"
	"github.com/servekit/license-service/pkg/config"
	"github.com/servekit/license-service/pkg/option"
)

const (
	testSeed    = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
	testPubB64  = "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="
	fingerprint = "v1.5555555555555555555555555555555555555555555555555555555555555555"
	keyShape    = "AV1DABCDEFGHIJKLMNOPQRST"
)

func tok(i int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i) }

type stack struct {
	client *pkg.Client
	db     *gorm.DB
	rdb    *redis.Client
	// appCtx carries calling-app credentials for the client surface
	// (Activate/Deactivate/TrialStart verify them, fail-closed).
	appCtx context.Context
}

func freePort(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	require.NoError(t, lis.Close())
	return addr
}

func startStack(t *testing.T) *stack {
	t.Helper()

	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, dbx.AutoMigrate(db, models.AllModels()...))
	rdb := redisx.NewTestClient(t)

	grpcAddr := freePort(t)
	cfg := &config.Config{
		Server:  &config.ServerConfig{GRPCAddr: grpcAddr},
		Signing: &config.SigningConfig{Keys: []*config.SigningKey{{KeyID: "k1", Seed: testSeed}}},
		Trial:   &config.TrialConfig{Days: 14},
		// Quota 1 pins the 429 path within a single test flow.
		RateLimit: &config.RateLimitConfig{KeyPrefix: "itest:rate", Window: time.Minute, Max: 1},
	}

	server, err := pkg.NewServer(cfg,
		pkg.WithServiceOptions(
			option.WithDB(db),
			option.WithRedis(rdb),
		))
	require.NoError(t, err)
	require.NoError(t, server.Start())
	t.Cleanup(func() { _ = server.Stop() })

	client, err := pkg.NewClient(grpcAddr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	// Wait for the listener to accept.
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, derr := net.DialTimeout("tcp", grpcAddr, 200*time.Millisecond)
		if derr == nil {
			_ = conn.Close()
			break
		}
		require.True(t, time.Now().Before(deadline), "gRPC server never came up")
	}

	// The client surface requires calling-app credentials (x-app-key /
	// x-app-secret) — seed one like an embedder would.
	const appKey, appSecret = "testkit", "lic_itest_secret"
	require.NoError(t, db.Create(&models.LicenseApp{
		AppKey: appKey, AppSecret: appSecret, Name: "integration",
	}).Error)
	appCtx := pkg.WithApp(context.Background(), appKey, appSecret)

	return &stack{client: client, db: db, rdb: rdb, appCtx: appCtx}
}

// seedKeyWithSlots creates a key via the admin surface and activates slots
// distinct device tokens.
func (s *stack) fillSlots(t *testing.T, key, plaintext string, slots int) {
	t.Helper()
	for i := 1; i <= slots; i++ {
		_, err := s.client.Activate(s.appCtx, &licensev1.ActivateRequest{
			Key: plaintext, FingerprintId: fingerprint, DeviceToken: tok(i),
		})
		require.NoError(t, err)
		require.NoError(t, s.rdb.FlushAll(context.Background()).Err())
	}
	_ = key
}

// TestErrorContract_StatusCodesAndDetails: the six license errors land on the
// exact gRPC codes, with SlotLimitInfo / RetryAfterInfo riding the status
// details.
func TestErrorContract_StatusCodesAndDetails(t *testing.T) {
	s := startStack(t)
	ctx := s.appCtx

	// 400 BAD_KEY_FORMAT → InvalidArgument.
	_, err := s.client.Activate(ctx, &licensev1.ActivateRequest{
		FingerprintId: fingerprint, DeviceToken: tok(1),
	})
	require.Equal(t, codes.InvalidArgument, status.Convert(err).Code())

	// 401 KEY_NOT_FOUND → Unauthenticated.
	_, err = s.client.Activate(ctx, &licensev1.ActivateRequest{
		Key: keyShape, FingerprintId: fingerprint, DeviceToken: tok(1),
	})
	require.Equal(t, codes.Unauthenticated, status.Convert(err).Code())

	created, err := s.client.CreateKey(context.Background(), &licensev1.CreateKeyRequest{
		Grants: []*licensev1.EntitlementInput{
			{Module: licensev1.Module_MODULE_TOOLS, Kind: licensev1.EntitlementKind_ENTITLEMENT_KIND_PERPETUAL},
		},
	})
	require.NoError(t, err)
	plaintext := created.GetPlaintextKey()

	// 429 RATE_LIMITED → ResourceExhausted + RetryAfterInfo.
	_, err = s.client.Activate(ctx, &licensev1.ActivateRequest{
		Key: plaintext, FingerprintId: fingerprint, DeviceToken: tok(1),
	})
	require.NoError(t, err)
	_, err = s.client.Activate(ctx, &licensev1.ActivateRequest{
		Key: plaintext, FingerprintId: fingerprint, DeviceToken: tok(1),
	})
	require.Equal(t, codes.ResourceExhausted, status.Convert(err).Code())
	var retryAfter int32
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*licensev1.RetryAfterInfo); ok {
			retryAfter = info.GetSeconds()
		}
	}
	require.EqualValues(t, 60, retryAfter)

	// 409 SLOT_LIMIT → AlreadyExists + SlotLimitInfo (roster attached).
	require.NoError(t, s.rdb.FlushAll(ctx).Err())
	s.fillSlots(t, created.GetKey().GetLicenseId(), plaintext, 3)
	_, err = s.client.Activate(ctx, &licensev1.ActivateRequest{
		Key: plaintext, FingerprintId: fingerprint, DeviceToken: tok(9),
	})
	st := status.Convert(err)
	require.Equal(t, codes.AlreadyExists, st.Code())
	var slots *licensev1.SlotLimitInfo
	for _, d := range st.Details() {
		if info, ok := d.(*licensev1.SlotLimitInfo); ok {
			slots = info
		}
	}
	require.NotNil(t, slots, "SlotLimitInfo detail must survive the wire")
	require.EqualValues(t, 3, slots.GetMaxSlots())
	require.Len(t, slots.GetDevices(), 3)

	// 403 KEY_REVOKED → PermissionDenied.
	_, err = s.client.RevokeKey(context.Background(), &licensev1.RevokeKeyRequest{KeyId: created.GetKey().GetLicenseId()})
	require.NoError(t, err)
	require.NoError(t, s.rdb.FlushAll(ctx).Err())
	_, err = s.client.Activate(ctx, &licensev1.ActivateRequest{
		Key: plaintext, FingerprintId: fingerprint, DeviceToken: tok(1),
	})
	require.Equal(t, codes.PermissionDenied, status.Convert(err).Code())

	// 400 ALREADY_ENTITLED → InvalidArgument (trial with a valid key grant).
	_, err = s.client.UnrevokeKey(context.Background(), &licensev1.UnrevokeKeyRequest{KeyId: created.GetKey().GetLicenseId()})
	require.NoError(t, err)
	require.NoError(t, s.rdb.FlushAll(ctx).Err())
	_, err = s.client.TrialStart(ctx, &licensev1.TrialStartRequest{
		Module: "tools", FingerprintId: fingerprint, DeviceToken: tok(1), LicenseKey: plaintext,
	})
	require.Equal(t, codes.InvalidArgument, status.Convert(err).Code())
}

// TestPayloadContract: the wire payload is canonical and the signature
// verifies against the pinned test public key; both key field names work.
func TestPayloadContract(t *testing.T) {
	s := startStack(t)
	ctx := s.appCtx

	created, err := s.client.CreateKey(context.Background(), &licensev1.CreateKeyRequest{
		Grants: []*licensev1.EntitlementInput{
			{Module: licensev1.Module_MODULE_DOWNLOADS, Kind: licensev1.EntitlementKind_ENTITLEMENT_KIND_PERPETUAL},
		},
	})
	require.NoError(t, err)

	// Dual field names: `key` …
	resp, err := s.client.Activate(ctx, &licensev1.ActivateRequest{
		Key: created.GetPlaintextKey(), FingerprintId: fingerprint, DeviceToken: tok(1),
	})
	require.NoError(t, err)

	// … and `license_key` (protojson maps both; on the gRPC wire the alias
	// field is set directly).
	require.NoError(t, s.rdb.FlushAll(ctx).Err())
	respAlias, err := s.client.Activate(ctx, &licensev1.ActivateRequest{
		LicenseKey: created.GetPlaintextKey(), FingerprintId: fingerprint, DeviceToken: tok(2),
	})
	require.NoError(t, err)

	for _, r := range []*licensev1.ActivateResponse{resp, respAlias} {
		parsed, perr := cert.UnmarshalCanonical([]byte(r.GetPayload()))
		require.NoError(t, perr, "payload must be canonical (client re-derives it byte-exactly)")
		require.Equal(t, created.GetKey().GetLicenseId(), *parsed.LicenseID)

		pub, _ := base64.StdEncoding.DecodeString(testPubB64)
		require.True(t, cert.Verify(ed25519.PublicKey(pub), r.GetPayload(), r.GetSignature()),
			"detached signature must verify with the pinned public key")
	}
}

// TestAdminFromInternalNetwork: the admin surface is callable directly —
// authorization is the edge's job (gateway + user/permission system); the
// only hard rule is that the gRPC port never leaves the internal network.
func TestAdminFromInternalNetwork(t *testing.T) {
	s := startStack(t)
	ctx := s.appCtx

	_, err := s.client.CreateKey(ctx, &licensev1.CreateKeyRequest{})
	require.NoError(t, err)

	// The client surface keeps its own error semantics.
	_, err = s.client.Activate(ctx, &licensev1.ActivateRequest{
		Key: "AV1DZZZZZZZZZZZZZZZZZZZZ", FingerprintId: fingerprint, DeviceToken: tok(1),
	})
	require.Equal(t, codes.Unauthenticated, status.Convert(err).Code(), "unknown key, not auth failure")
}

// TestHealth: both checks green → SERVING; a dead DB flips to Unavailable.
func TestHealth(t *testing.T) {
	s := startStack(t)
	ctx := s.appCtx

	resp, err := s.client.Health(ctx, &licensev1.HealthRequest{})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.GetStatus())
	require.True(t, resp.GetChecks().GetDb())
	require.True(t, resp.GetChecks().GetSigning())

	sqlDB, err := s.db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	// The stopped pool must flip the RPC to Unavailable (503 via gateway).
	require.Eventually(t, func() bool {
		_, herr := s.client.Health(ctx, &licensev1.HealthRequest{})
		return errors.Is(herr, context.DeadlineExceeded) ||
			status.Convert(herr).Code() == codes.Unavailable
	}, 5*time.Second, 100*time.Millisecond)
}

// TestProtovalidateOnTheWire: malformed fingerprint is rejected before any
// business code runs.
func TestProtovalidateOnTheWire(t *testing.T) {
	s := startStack(t)
	_, err := s.client.Activate(context.Background(), &licensev1.ActivateRequest{
		Key: keyShape, FingerprintId: "not-a-fingerprint", DeviceToken: tok(1),
	})
	require.Equal(t, codes.InvalidArgument, status.Convert(err).Code())
}
