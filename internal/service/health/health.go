// Package health implements the GET /healthz probe semantics: DB
// reachability (SELECT 1) plus signing-key readiness, mapping any failure to
// ServiceUnavailable (HTTP 503 via the future gateway). External uptime
// probes fire every 2 minutes and alert after 3 consecutive failures — an
// unreachable activation service is a P1 event (design doc §11).
package health

import (
	"context"

	"gorm.io/gorm"

	licensev1 "github.com/servekit/license-service/gen/license/v1"
	"github.com/servekit/license-service/internal/service/cert"
	"github.com/servekit/license-service/pkg/xcodes"
)

// Service probes the runtime dependencies.
type Service struct {
	db     *gorm.DB
	signer *cert.Signer
}

// New constructs the health domain.
func New(db *gorm.DB, signer *cert.Signer) *Service {
	return &Service{db: db, signer: signer}
}

// Health checks both dependencies; any failure flips the whole RPC to
// Unavailable so probes never see a green face over a dead dependency.
func (s *Service) Health(ctx context.Context, _ *licensev1.HealthRequest) (*licensev1.HealthResponse, error) {
	checks := &licensev1.HealthChecks{}
	status := "ok"

	var one int
	if err := s.db.WithContext(ctx).Raw("SELECT 1").Scan(&one).Error; err != nil || one != 1 {
		checks.Db = false
		status = "unavailable"
	} else {
		checks.Db = true
	}
	checks.Signing = s.signer.Ready()
	if !checks.Signing {
		status = "unavailable"
	}

	if status != "ok" {
		return nil, xcodes.ErrServiceUnavailable.New(status)
	}
	return &licensev1.HealthResponse{Status: status, Checks: checks}, nil
}
