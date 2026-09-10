// Data-plane caller authentication for the client-facing RPCs. Phase ③
// dual-stack window: the pre-③ appauth point became tenantres.Require —
// trusted x-tenant-key is authoritative (gate row lazily upserted on first
// sight), legacy x-app-key/x-app-secret validation is unchanged with the
// validated app converted to its mapped tenant_key, neither present fails
// closed. Mirrors message-service / storage-service: credentials ride
// metadata and are verified in the service layer — not an interceptor — so
// module-mode in-process callers share the same path as gRPC clients. Reads
// go through the DB on every call (activation is not hot enough to warrant a
// registry snapshot).
package service

import (
	"context"

	"github.com/servekit/license-service/internal/tenantres"
)

// requireCaller resolves and verifies the data-plane caller, failing closed
// with ErrAppUnauthorized on missing credentials, unknown/disabled gate row,
// or a bad secret. The returned Caller's gate check is the whole effect —
// license data itself is global (no tenant dimension).
func (s *Service) requireCaller(ctx context.Context) (*tenantres.Caller, error) {
	return s.tenantRes.Require(ctx)
}
