package pkg

import (
	"context"
	"fmt"

	commonv1 "github.com/servekit/api/gen/go/common/v1"
	licensev1 "github.com/servekit/api/gen/go/license/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Client is a gRPC client for license-service shaped like *Handler: it implements
// both generated server interfaces (unary methods without grpc.CallOption),
// so a consumer can hold either backend behind the provider-defined Service
// interface — module mode passes the *Handler, grpc mode passes the *Client —
// with no per-consumer adapter.
//
// The Unimplemented embeds satisfy the mustEmbed guards; every RPC below
// shadows them with a real delegation. When a new RPC is added to the proto,
// add its delegation here — until then grpc mode returns codes.Unimplemented
// for it.
type Client struct {
	licensev1.UnimplementedLicenseServiceServer
	licensev1.UnimplementedLicenseAdminServiceServer

	conn                *grpc.ClientConn
	LicenseService      licensev1.LicenseServiceClient
	LicenseAdminService licensev1.LicenseAdminServiceClient
}

// Compile-time assertions: *Client and *Handler expose the same interfaces.
var _ licensev1.LicenseServiceServer = (*Client)(nil)
var _ licensev1.LicenseAdminServiceServer = (*Client)(nil)

// NewClient dials license-service at addr using insecure credentials by default.
// Pass additional DialOptions (e.g., credentials) to override.
func NewClient(addr string, opts ...grpc.DialOption) (*Client, error) {
	dialOpts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, opts...)

	conn, err := grpc.NewClient(addr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	return &Client{
		conn:                conn,
		LicenseService:      licensev1.NewLicenseServiceClient(conn),
		LicenseAdminService: licensev1.NewLicenseAdminServiceClient(conn),
	}, nil
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error { return c.conn.Close() }

// Ping delegates to the remote license-service.
func (c *Client) Ping(ctx context.Context, in *emptypb.Empty) (*commonv1.Pong, error) {
	return c.LicenseService.Ping(ctx, in)
}

// Activate delegates to the remote license-service.
func (c *Client) Activate(ctx context.Context, in *licensev1.ActivateRequest) (*licensev1.ActivateResponse, error) {
	return c.LicenseService.Activate(ctx, in)
}

// Deactivate delegates to the remote license-service.
func (c *Client) Deactivate(ctx context.Context, in *licensev1.DeactivateRequest) (*licensev1.DeactivateResponse, error) {
	return c.LicenseService.Deactivate(ctx, in)
}

// TrialStart delegates to the remote license-service.
func (c *Client) TrialStart(ctx context.Context, in *licensev1.TrialStartRequest) (*licensev1.TrialStartResponse, error) {
	return c.LicenseService.TrialStart(ctx, in)
}

// Health delegates to the remote license-service.
func (c *Client) Health(ctx context.Context, in *licensev1.HealthRequest) (*licensev1.HealthResponse, error) {
	return c.LicenseService.Health(ctx, in)
}

// CreateKey delegates to the remote license-service.
func (c *Client) CreateKey(ctx context.Context, in *licensev1.CreateKeyRequest) (*licensev1.CreateKeyResponse, error) {
	return c.LicenseAdminService.CreateKey(ctx, in)
}

// ShowKey delegates to the remote license-service.
func (c *Client) ShowKey(ctx context.Context, in *licensev1.ShowKeyRequest) (*licensev1.ShowKeyResponse, error) {
	return c.LicenseAdminService.ShowKey(ctx, in)
}

// ListKeys delegates to the remote license-service.
func (c *Client) ListKeys(ctx context.Context, in *licensev1.ListKeysRequest) (*licensev1.ListKeysResponse, error) {
	return c.LicenseAdminService.ListKeys(ctx, in)
}

// UpdateKey delegates to the remote license-service.
func (c *Client) UpdateKey(ctx context.Context, in *licensev1.UpdateKeyRequest) (*licensev1.UpdateKeyResponse, error) {
	return c.LicenseAdminService.UpdateKey(ctx, in)
}

// RevokeKey delegates to the remote license-service.
func (c *Client) RevokeKey(ctx context.Context, in *licensev1.RevokeKeyRequest) (*licensev1.RevokeKeyResponse, error) {
	return c.LicenseAdminService.RevokeKey(ctx, in)
}

// UnrevokeKey delegates to the remote license-service.
func (c *Client) UnrevokeKey(ctx context.Context, in *licensev1.UnrevokeKeyRequest) (*licensev1.UnrevokeKeyResponse, error) {
	return c.LicenseAdminService.UnrevokeKey(ctx, in)
}

// DeleteKey delegates to the remote license-service.
func (c *Client) DeleteKey(ctx context.Context, in *licensev1.DeleteKeyRequest) (*licensev1.DeleteKeyResponse, error) {
	return c.LicenseAdminService.DeleteKey(ctx, in)
}

// GrantModule delegates to the remote license-service.
func (c *Client) GrantModule(ctx context.Context, in *licensev1.GrantModuleRequest) (*licensev1.GrantModuleResponse, error) {
	return c.LicenseAdminService.GrantModule(ctx, in)
}

// RevokeModule delegates to the remote license-service.
func (c *Client) RevokeModule(ctx context.Context, in *licensev1.RevokeModuleRequest) (*licensev1.RevokeModuleResponse, error) {
	return c.LicenseAdminService.RevokeModule(ctx, in)
}

// ListKeyDevices delegates to the remote license-service.
func (c *Client) ListKeyDevices(ctx context.Context, in *licensev1.ListKeyDevicesRequest) (*licensev1.ListKeyDevicesResponse, error) {
	return c.LicenseAdminService.ListKeyDevices(ctx, in)
}

// KickDevice delegates to the remote license-service.
func (c *Client) KickDevice(ctx context.Context, in *licensev1.KickDeviceRequest) (*licensev1.KickDeviceResponse, error) {
	return c.LicenseAdminService.KickDevice(ctx, in)
}

// ShowTrial delegates to the remote license-service.
func (c *Client) ShowTrial(ctx context.Context, in *licensev1.ShowTrialRequest) (*licensev1.ShowTrialResponse, error) {
	return c.LicenseAdminService.ShowTrial(ctx, in)
}

// ResetTrial delegates to the remote license-service.
func (c *Client) ResetTrial(ctx context.Context, in *licensev1.ResetTrialRequest) (*licensev1.ResetTrialResponse, error) {
	return c.LicenseAdminService.ResetTrial(ctx, in)
}

// ShowPubKey delegates to the remote license-service.
func (c *Client) ShowPubKey(ctx context.Context, in *licensev1.ShowPubKeyRequest) (*licensev1.ShowPubKeyResponse, error) {
	return c.LicenseAdminService.ShowPubKey(ctx, in)
}
