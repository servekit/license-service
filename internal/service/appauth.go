// Data-plane app authentication for the client-facing RPCs. Mirrors
// message-service / storage-service: credentials ride x-app-key /
// x-app-secret metadata and are verified in the service layer — not an
// interceptor — so module-mode in-process callers share the same path as
// gRPC clients. Verification reads the DB on every call (activation is not
// hot enough to warrant a registry snapshot).
package service

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/servekit/license-service/internal/appauth"
	"github.com/servekit/license-service/internal/store/dal"
	"github.com/servekit/license-service/internal/store/models"
	"github.com/servekit/license-service/pkg/xcodes"
)

// requireApp resolves and verifies the calling app, failing closed with
// ErrAppUnauthorized on missing credentials, unknown/disabled app, or a bad
// secret.
func (s *Service) requireApp(ctx context.Context) (*models.LicenseApp, error) {
	appKey, appSecret, ok := appauth.Credentials(ctx)
	if !ok {
		return nil, xcodes.ErrAppUnauthorized.New("missing app credentials (x-app-key / x-app-secret metadata)")
	}
	app, err := dal.GetAppByKey(ctx, s.db, appKey)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, xcodes.ErrAppUnauthorized.New("unknown app credentials")
	}
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	if app.Disabled {
		return nil, xcodes.ErrAppUnauthorized.New("app is disabled")
	}
	if app.AppSecret != appSecret {
		return nil, xcodes.ErrAppUnauthorized.New("invalid app secret")
	}
	return app, nil
}
