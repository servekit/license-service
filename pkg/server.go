// Package pkg is the public surface of license-service. It wires internal pieces
// into a runnable gRPC/HTTP server, a gRPC client, and an in-process module
// entry point. Downstream services that depend on license-service should import
// this package — not internal/*.
package pkg

import (
	"errors"

	"buf.build/go/protovalidate"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"google.golang.org/grpc"

	"github.com/servekit/go-common/grpcx"
	"github.com/servekit/go-common/signalx"

	licensev1 "github.com/servekit/api/gen/go/license/v1"
	"github.com/servekit/license-service/internal/service"
	"github.com/servekit/license-service/pkg/config"
	"github.com/servekit/license-service/pkg/handler"
	"github.com/servekit/license-service/pkg/interceptor"
	"github.com/servekit/license-service/pkg/option"
)

// Compile-time assertion: *Server satisfies signalx.Service.
var _ signalx.Service = (*Server)(nil)

// Server wraps a gRPC + HTTP gateway server for the license service.
//
// Holds grpcSrv (gRPC + gateway transports) and hdl (the Handler, which
// itself wraps the underlying *service.Service and exposes Start/Stop).
// There's no separate svc field — Handler is the single handle for both
// RPC dispatch and lifecycle.
type Server struct {
	grpcSrv *grpcx.Server
	hdl     *handler.Handler
}

// ServerOption configures a Server instance.
type ServerOption func(*serverOptions)

type serverOptions struct {
	serviceOpts []option.Option
}

// WithServiceOptions forwards options to the service layer.
func WithServiceOptions(opts ...option.Option) ServerOption {
	return func(o *serverOptions) { o.serviceOpts = append(o.serviceOpts, opts...) }
}

// NewServer constructs a Server with all dependencies wired.
//
// The gRPC server runs with three interceptors in order:
//   - interceptor.Error: maps xerr-wrapped service errors to gRPC status
//     codes ("REASON: message" preserved) and promotes xcodes.Detailed
//     proto details (SlotLimitInfo / RetryAfterInfo) into the status
//   - protovalidate.UnaryServerInterceptor: enforces (buf.validate.field)
//     rules declared in license.proto
//
// Authorization lives at the edge (gateway + user/permission system):
// license-service trusts its internal network boundary and holds no
// operator-identity notion of its own.
//
// license-service is gRPC-only: registerGW is nil and no HTTP gateway runs
// in-process. The client-facing HTTP surface is served by the gateway
// (testkit today; a standalone gateway later) — the gateway owns its own
// HTTP route mapping (see docs/wire-contract.md).
func NewServer(cfg *config.Config, opts ...ServerOption) (*Server, error) {
	var so serverOptions
	for _, opt := range opts {
		opt(&so)
	}

	svc, err := service.New(cfg, so.serviceOpts...)
	if err != nil {
		return nil, err
	}

	hdl := handler.New(svc)

	validator, err := protovalidate.New()
	if err != nil {
		return nil, err
	}

	grpcSrv := grpcx.New(
		&grpcx.ServerConfig{GRPCAddr: cfg.Server.GRPCAddr},
		func(gs *grpc.Server) {
			licensev1.RegisterLicenseServiceServer(gs, hdl)
			licensev1.RegisterLicenseAdminServiceServer(gs, hdl)
		},
		nil,
		interceptor.Error,
		grpcx.TrustedActorUnary(),
		protovalidate_middleware.UnaryServerInterceptor(validator),
	)

	return &Server{grpcSrv: grpcSrv, hdl: hdl}, nil
}

// Start starts service internals and the gRPC + HTTP gateway without blocking.
//
// On partial failure, started components are rolled back via Stop.
func (s *Server) Start() error {
	if err := s.hdl.Start(); err != nil {
		return err
	}
	if err := s.grpcSrv.Start(); err != nil {
		return errors.Join(err, s.hdl.Stop())
	}
	return nil
}

// Stop gracefully stops the gRPC + HTTP gateway and service internals.
// Errors from each component are aggregated via errors.Join.
func (s *Server) Stop() error {
	return errors.Join(s.grpcSrv.Stop(), s.hdl.Stop())
}
