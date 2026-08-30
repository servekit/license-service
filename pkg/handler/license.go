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
