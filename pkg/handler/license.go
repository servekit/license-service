// Package handler implements license.v1.LicenseServiceServer as a thin shim over
// internal/service. Each method is a one-line delegation — service takes the
// proto request directly (convert at the store boundary, not here).
//
// Handlers hold NO business logic and NO conversion logic. Anything beyond
// `return h.svc.X(ctx, req)` belongs in internal/service.
//
// Handler also implements signalx.Service (Start/Stop) by delegating to the
// underlying Service, so in-process module users manage lifecycle via the same
// object they call RPC methods on.
package handler

import (
	"context"

	licensev1 "github.com/servekit/license-service/gen/license/v1"
	"github.com/servekit/license-service/internal/service"

	"google.golang.org/protobuf/types/known/emptypb"
)

// Handler implements license.v1.LicenseServiceServer and
// license.v1.LicenseAdminServiceServer. It holds no mutable state — the
// embedded *service.Service owns all business state and lifecycle.
type Handler struct {
	licensev1.UnimplementedLicenseServiceServer
	licensev1.UnimplementedLicenseAdminServiceServer

	svc *service.Service
}

// New constructs a Handler wrapping svc.
func New(svc *service.Service) *Handler {
	return &Handler{svc: svc}
}

// Compile-time assertions: Handler implements both gRPC server interfaces.
var (
	_ licensev1.LicenseServiceServer      = (*Handler)(nil)
	_ licensev1.LicenseAdminServiceServer = (*Handler)(nil)
)

// Start starts service-internal components (background goroutines for owned
// resources like cron, message consumers, etc.).
func (h *Handler) Start() error { return h.svc.Start() }

// Stop releases resources owned by the service. After Stop, the Handler must
// not be used.
func (h *Handler) Stop() error { return h.svc.Stop() }

// Ping is a health-check RPC, always generated so the grpc-gateway has at
// least one HTTP endpoint and pkg/server.go can always register the gateway
// handler. (A proto service with zero RPCs produces no HandlerFromEndpoint,
// which would silently disable the HTTP gateway.)
func (h *Handler) Ping(ctx context.Context, _ *emptypb.Empty) (*licensev1.Pong, error) {
	return h.svc.Ping(ctx)
}

// Activate is the idempotent client converger (first activation, heartbeat,
// fingerprint rebind, refresh, evict-retry).
func (h *Handler) Activate(ctx context.Context, req *licensev1.ActivateRequest) (*licensev1.ActivateResponse, error) {
	return h.svc.Activate(ctx, req)
}

// Deactivate releases the caller's own slot; always 200 (released=false when
// never in a slot).
func (h *Handler) Deactivate(ctx context.Context, req *licensev1.DeactivateRequest) (*licensev1.DeactivateResponse, error) {
	return h.svc.Deactivate(ctx, req)
}

// TrialStart starts or resumes a keyless trial (fingerprint-anchored).
func (h *Handler) TrialStart(ctx context.Context, req *licensev1.TrialStartRequest) (*licensev1.TrialStartResponse, error) {
	return h.svc.TrialStart(ctx, req)
}

// ─── admin delegations ──────────────────────────────────────────────────────

// CreateKey mints a key; the plaintext appears exactly once in the response.
func (h *Handler) CreateKey(ctx context.Context, req *licensev1.CreateKeyRequest) (*licensev1.CreateKeyResponse, error) {
	return h.svc.CreateKey(ctx, req)
}

// ShowKey returns the full key view with the slot roster.
func (h *Handler) ShowKey(ctx context.Context, req *licensev1.ShowKeyRequest) (*licensev1.ShowKeyResponse, error) {
	return h.svc.ShowKey(ctx, req)
}

// ListKeys returns the key roster (status 0 = all).
func (h *Handler) ListKeys(ctx context.Context, req *licensev1.ListKeysRequest) (*licensev1.ListKeysResponse, error) {
	return h.svc.ListKeys(ctx, req)
}

// UpdateKey applies optional label/slots updates.
func (h *Handler) UpdateKey(ctx context.Context, req *licensev1.UpdateKeyRequest) (*licensev1.UpdateKeyResponse, error) {
	return h.svc.UpdateKey(ctx, req)
}

// RevokeKey freezes the key soft-state.
func (h *Handler) RevokeKey(ctx context.Context, req *licensev1.RevokeKeyRequest) (*licensev1.RevokeKeyResponse, error) {
	return h.svc.RevokeKey(ctx, req)
}

// UnrevokeKey reactivates a revoked key.
func (h *Handler) UnrevokeKey(ctx context.Context, req *licensev1.UnrevokeKeyRequest) (*licensev1.UnrevokeKeyResponse, error) {
	return h.svc.UnrevokeKey(ctx, req)
}

// DeleteKey physically deletes the key with app-level cascade.
func (h *Handler) DeleteKey(ctx context.Context, req *licensev1.DeleteKeyRequest) (*licensev1.DeleteKeyResponse, error) {
	return h.svc.DeleteKey(ctx, req)
}

// GrantModule upserts one module entitlement.
func (h *Handler) GrantModule(ctx context.Context, req *licensev1.GrantModuleRequest) (*licensev1.GrantModuleResponse, error) {
	return h.svc.GrantModule(ctx, req)
}

// RevokeModule removes one module entitlement.
func (h *Handler) RevokeModule(ctx context.Context, req *licensev1.RevokeModuleRequest) (*licensev1.RevokeModuleResponse, error) {
	return h.svc.RevokeModule(ctx, req)
}

// ListKeyDevices returns the slot roster of a key.
func (h *Handler) ListKeyDevices(ctx context.Context, req *licensev1.ListKeyDevicesRequest) (*licensev1.ListKeyDevicesResponse, error) {
	return h.svc.ListKeyDevices(ctx, req)
}

// KickDevice force-releases one slot (support-side eviction).
func (h *Handler) KickDevice(ctx context.Context, req *licensev1.KickDeviceRequest) (*licensev1.KickDeviceResponse, error) {
	return h.svc.KickDevice(ctx, req)
}

// ShowTrial returns a fingerprint's trial ledger.
func (h *Handler) ShowTrial(ctx context.Context, req *licensev1.ShowTrialRequest) (*licensev1.ShowTrialResponse, error) {
	return h.svc.ShowTrial(ctx, req)
}

// ResetTrial deletes one trial ledger row (manual reset channel).
func (h *Handler) ResetTrial(ctx context.Context, req *licensev1.ResetTrialRequest) (*licensev1.ResetTrialResponse, error) {
	return h.svc.ResetTrial(ctx, req)
}

// ShowPubKey exposes the signing public key(s) for client pinning.
func (h *Handler) ShowPubKey(ctx context.Context, req *licensev1.ShowPubKeyRequest) (*licensev1.ShowPubKeyResponse, error) {
	return h.svc.ShowPubKey(ctx, req)
}

// Health reports DB and signing readiness (GET /healthz).
func (h *Handler) Health(ctx context.Context, req *licensev1.HealthRequest) (*licensev1.HealthResponse, error) {
	return h.svc.Health(ctx, req)
}
