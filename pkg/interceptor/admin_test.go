package interceptor_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/stretchr/testify/require"

	"github.com/servekit/license-service/pkg/interceptor"
)

const testToken = "opensesame"

func bearerCtx(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+token))
}

func callAdmin(ctx context.Context) error {
	_, err := interceptor.AdminAuth(testToken)(ctx, nil,
		&grpc.UnaryServerInfo{FullMethod: "/license.v1.LicenseAdminService/CreateKey"},
		func(context.Context, any) (any, error) { return "ok", nil })
	return err
}

// TestAdminAuth_AllowsClientServiceWithoutToken: non-admin methods are never
// gated.
func TestAdminAuth_AllowsClientServiceWithoutToken(t *testing.T) {
	_, err := interceptor.AdminAuth(testToken)(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/license.v1.LicenseService/Activate"},
		func(context.Context, any) (any, error) { return "ok", nil })
	require.NoError(t, err)
}

// TestAdminAuth_ValidTokenPasses.
func TestAdminAuth_ValidTokenPasses(t *testing.T) {
	require.NoError(t, callAdmin(bearerCtx(testToken)))
}

// TestAdminAuth_MissingOrWrongToken: 401-mapping Unauthenticated.
func TestAdminAuth_MissingOrWrongToken(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no metadata": context.Background(),
		"wrong token": bearerCtx("letmein"),
		"empty token": bearerCtx(""),
		"bad scheme":  metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Basic xyz")),
	} {
		t.Run(name, func(t *testing.T) {
			err := callAdmin(ctx)
			require.Error(t, err)
			require.Equal(t, codes.Unauthenticated, status.Convert(err).Code())
		})
	}
}

// TestAdminAuth_EmptyConfiguredTokenFailsClosed: when no admin token is
// configured, every admin RPC is rejected.
func TestAdminAuth_EmptyConfiguredTokenFailsClosed(t *testing.T) {
	_, err := interceptor.AdminAuth("")(bearerCtx(""), nil,
		&grpc.UnaryServerInfo{FullMethod: "/license.v1.LicenseAdminService/CreateKey"},
		func(context.Context, any) (any, error) { return "ok", nil })
	require.Error(t, err)
	require.Equal(t, codes.Unauthenticated, status.Convert(err).Code())
}
