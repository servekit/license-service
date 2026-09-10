// App-credential helpers for embedders: wrap a context with the caller's
// credential metadata before invoking client-facing license RPCs (Activate /
// Deactivate / TrialStart). Mirrors message-service and storage-service.
package pkg

import (
	"context"

	"github.com/servekit/license-service/internal/appauth"
)

// WithApp plants legacy app credentials (x-app-key / x-app-secret) as
// incoming AND outgoing metadata on ctx.
func WithApp(ctx context.Context, appKey, appSecret string) context.Context {
	return appauth.WithApp(ctx, appKey, appSecret)
}

// WithTenant plants the trusted tenant key (x-tenant-key) as incoming AND
// outgoing metadata on ctx — the module-mode mirror of what the portal proxy
// does on the wire. Phase ③ dual-stack window: takes precedence over any
// legacy credentials on the same ctx (D-③1).
func WithTenant(ctx context.Context, tenantKey string) context.Context {
	return appauth.WithTenant(ctx, tenantKey)
}
