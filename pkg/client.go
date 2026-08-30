package pkg

import (
	licensev1 "github.com/servekit/license-service/gen/license/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client is a gRPC client for license-service.
//
// Embeds both service clients so callers can invoke RPCs directly:
//
//	c, _ := pkg.NewClient("localhost:19096")
//	resp, _ := c.Activate(ctx, &licensev1.ActivateRequest{...})
//	pub, _ := c.ShowPubKey(authCtx, &licensev1.ShowPubKeyRequest{})
//
// Admin RPCs additionally require `authorization: Bearer <token>` metadata
// (see interceptor.AdminAuth).
type Client struct {
	conn *grpc.ClientConn
	licensev1.LicenseServiceClient
	licensev1.LicenseAdminServiceClient
}

// NewClient dials license-service at addr using insecure credentials by default.
// Pass additional DialOptions (e.g., credentials) to override.
func NewClient(addr string, opts ...grpc.DialOption) (*Client, error) {
	dialOpts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, opts...)

	conn, err := grpc.NewClient(addr, dialOpts...)
	if err != nil {
		return nil, err
	}

	return &Client{
		conn:                      conn,
		LicenseServiceClient:      licensev1.NewLicenseServiceClient(conn),
		LicenseAdminServiceClient: licensev1.NewLicenseAdminServiceClient(conn),
	}, nil
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error { return c.conn.Close() }
