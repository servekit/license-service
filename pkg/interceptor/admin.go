package interceptor

import (
	"context"
	"crypto/subtle"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/servekit/go-common/grpcx"
)

// adminMethodPrefix gates every RPC of the admin service.
const adminMethodPrefix = "/license.v1.LicenseAdminService/"

// AdminAuth returns a unary interceptor that enforces
// `Authorization: Bearer <token>` on LicenseAdminService methods. Other
// methods pass through untouched.
//
// When token is empty every admin RPC is rejected (fail-closed) — a missing
// ADMIN_TOKEN must never expose the admin surface. Comparison is
// constant-time. The deployment layer keeps admin paths off the public edge;
// this interceptor is the second, independent gate.
func AdminAuth(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !strings.HasPrefix(info.FullMethod, adminMethodPrefix) {
			return handler(ctx, req)
		}
		if token == "" {
			return nil, status.Error(codes.Unauthenticated, "admin token not configured")
		}
		got, err := grpcx.BearerTokenFromCtx(ctx)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "admin authorization required")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "invalid admin token")
		}
		return handler(ctx, req)
	}
}
