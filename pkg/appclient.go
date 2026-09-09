// App-credential helpers for embedders: wrap a context with the calling
// app's x-app-key / x-app-secret metadata before invoking client-facing
// license RPCs (Activate / Deactivate / TrialStart). Mirrors message-service
// and storage-service.
package pkg

import (
	"context"

	"github.com/servekit/license-service/internal/appauth"
)

// WithApp plants app credentials as incoming metadata on ctx.
func WithApp(ctx context.Context, appKey, appSecret string) context.Context {
	return appauth.WithApp(ctx, appKey, appSecret)
}
