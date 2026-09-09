// Package appauth carries calling-app credentials (x-app-key / x-app-secret)
// through gRPC metadata. Verification happens in the service layer — not an
// interceptor — so module-mode in-process callers share the same path as
// gRPC clients. Mirrors message-service / storage-service.
package appauth

import (
	"context"

	"google.golang.org/grpc/metadata"
)

const (
	// appKeyKey is the metadata key carrying the calling app's key.
	appKeyKey = "x-app-key"
	// appSecretKey is the metadata key carrying the calling app's secret.
	appSecretKey = "x-app-secret"
)

// WithApp plants app credentials on ctx as BOTH incoming and outgoing
// metadata: incoming covers module-mode (in-process) calls where the context
// flows straight into the service impl; outgoing lets a real gRPC client
// forward them to a remote server.
func WithApp(ctx context.Context, appKey, appSecret string) context.Context {
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(
		appKeyKey, appKey,
		appSecretKey, appSecret,
	))
	return metadata.AppendToOutgoingContext(ctx, appKeyKey, appKey, appSecretKey, appSecret)
}

// Credentials extracts app credentials from INCOMING metadata. ok=false when
// either key is missing (the service layer rejects).
func Credentials(ctx context.Context) (appKey, appSecret string, ok bool) {
	md, exists := metadata.FromIncomingContext(ctx)
	if !exists {
		return "", "", false
	}
	keys := md.Get(appKeyKey)
	secrets := md.Get(appSecretKey)
	if len(keys) != 1 || len(secrets) != 1 || keys[0] == "" || secrets[0] == "" {
		return "", "", false
	}
	return keys[0], secrets[0], true
}
