// Data-plane caller authentication for the client-facing RPCs. Since the
// ④ window close tenantres.Require accepts ONLY the trusted x-tenant-key
// (format-validated; gate row lazily upserted on first sight; anything else
// fails closed). Mirrors message-service / storage-service: credentials
// ride metadata and are verified in the service layer — not an interceptor
// — so module-mode in-process callers share the same path as gRPC clients.
// Reads go through the DB on every call (activation is not hot enough to
// warrant a registry snapshot).
package service

import (
	"context"

	"github.com/servekit/license-service/internal/tenantres"
)

// requireCaller resolves and verifies the data-plane caller, failing closed
// with ErrAppUnauthorized on a missing/malformed credential or an
// unknown/disabled gate row. The returned Caller's gate check is the whole
// effect — license data itself is global (no tenant dimension).
func (s *Service) requireCaller(ctx context.Context) (*tenantres.Caller, error) {
	return s.tenantRes.Require(ctx)
}
