package pkg

import (
	"context"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"

	commonv1 "github.com/servekit/api/gen/go/common/v1"
	pb "github.com/servekit/api/gen/go/license/v1"
	"github.com/servekit/go-common/grpcx"
	"github.com/servekit/go-common/grpcx/clienttest"
	"github.com/servekit/go-common/tenantctx"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"
)

// smokeStub satisfies the server interface(s) with Ping overridden. The point
// of this test is the *Client's delegation routing, not the real service, so
// a stub avoids all handler fixtures (db/redis/providers).
type smokeStub struct {
	pb.UnimplementedLicenseServiceServer
	pb.UnimplementedLicenseAdminServiceServer
}

func (smokeStub) Ping(context.Context, *emptypb.Empty) (*commonv1.Pong, error) {
	return &commonv1.Pong{Status: "SERVING"}, nil
}

// captureStub records the incoming metadata of the last Ping — the wire-path
// view a downstream replica would see.
type captureStub struct {
	pb.UnimplementedLicenseServiceServer

	mu sync.Mutex
	md metadata.MD
}

func (s *captureStub) Ping(ctx context.Context, _ *emptypb.Empty) (*commonv1.Pong, error) {
	s.mu.Lock()
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		s.md = md
	}
	s.mu.Unlock()
	return &commonv1.Pong{Status: "SERVING"}, nil
}

// captured returns the recorded incoming metadata.
func (s *captureStub) captured() metadata.MD {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.md
}

// mdValue returns the first value for key, case-insensitively.
func mdValue(md metadata.MD, key string) string {
	for k, vals := range md {
		if len(vals) > 0 && strings.EqualFold(k, key) {
			return vals[0]
		}
	}
	return ""
}

// TestClient_GRPCRoundTrip drives the server-shaped *Client against a real
// in-process gRPC server: Ping asserts the wire path, then EveryUnary walks
// the whole interface — a self-recursive or mis-routed delegation (the bug
// class unit tests never reach) kills the test binary here.
func TestClient_GRPCRoundTrip(t *testing.T) {
	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	pb.RegisterLicenseServiceServer(gs, smokeStub{})
	pb.RegisterLicenseAdminServiceServer(gs, smokeStub{})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	c, err := NewClient(lis.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()

	pong, err := c.Ping(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	require.Equal(t, "SERVING", pong.GetStatus())

	clienttest.EveryUnary(ctx, t, c, reflect.TypeOf((*pb.LicenseServiceServer)(nil)).Elem())
	clienttest.EveryUnary(ctx, t, c, reflect.TypeOf((*pb.LicenseAdminServiceServer)(nil)).Elem())
}

// TestClient_ForwardsActorAndTenantOnRealHop: phase ④ grpc mode — a caller
// ctx carrying the verified actor (ctx value) and the trusted tenant key
// (tenantctx value only, exactly what the doors' lift produces post-transcode)
// must deliver BOTH across a real gRPC hop: x-actor for the admin plane's
// actor branches, x-tenant-key for the dualauth tenant stack. License's dial
// chain historically forwarded neither — identity and tenant died at the hop.
func TestClient_ForwardsActorAndTenantOnRealHop(t *testing.T) {
	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	stub := &captureStub{}
	gs := grpc.NewServer()
	pb.RegisterLicenseServiceServer(gs, stub)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	c, err := NewClient(lis.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	ctx := tenantctx.WithTenantKey(context.Background(), "ten_beta")
	ctx = grpcx.WithActor(ctx, &commonv1.RequestActor{UserId: 42, TenantKey: "ten_beta"})

	_, err = c.Ping(ctx, &emptypb.Empty{})
	require.NoError(t, err)

	md := stub.captured()
	require.Equal(t, "ten_beta", mdValue(md, tenantctx.HeaderTenantKey), "trusted tenant key must survive the real gRPC hop")

	actor, err := grpcx.UnmarshalActor(mdValue(md, grpcx.HeaderActor))
	require.NoError(t, err, "actor must be forwarded and wire-decodable")
	require.Equal(t, int64(42), actor.GetUserId())
}
