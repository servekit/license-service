// Package service contains license-service business logic.
//
// Layering contract (see golang-service-development skill §2):
//   - This is the SERVICE ROOT. It holds Service struct + New + Start/Stop +
//     one-line facade methods (one per RPC).
//   - Business logic lives in SUBPACKAGES (internal/service/<domain>/). This
//     file does NOT contain CRUD implementations — only delegations.
//   - handler calls service.X; service.X is a one-line facade that calls
//     s.<domain>.X in the subpackage. handler never imports the subpackage.
//   - Service methods take proto types DIRECTLY and return proto types — no
//     intermediate Go structs at any layer.
//   - Resources (db, redis, third-party services) are constructed here from
//     cfg (or injected via option) and passed to subpackage constructors. The
//     subpackages do NOT manage resource lifecycle — this Service does via
//     lifecycle.Manager.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"gorm.io/gorm"

	licensev1 "github.com/servekit/license-service/gen/license/v1"
	"github.com/servekit/license-service/internal/jobs"
	"github.com/servekit/license-service/internal/service/activation"
	"github.com/servekit/license-service/internal/version"

	"github.com/servekit/license-service/pkg/config"
	"github.com/servekit/license-service/pkg/option"

	"github.com/servekit/go-common/cronx"
	"github.com/servekit/go-common/lifecycle"
)

// Service holds license-service business state.
//
// Resource fields (db, redis) are convenience references kept on the root
// Service — they point at the same instances tracked by mgr and injected into
// subpackages. Each domain lives in its own subpackage field; subpackages do
// NOT reference this struct.
type Service struct {
	cfg *config.Config
	mgr *lifecycle.Manager
	db  *gorm.DB
	rdb *redis.Client

	// activation owns the client-facing activation domain (A1–A12).
	activation *activation.Service

	// startedAt is set once in New; Ping returns it for uptime.
	startedAt int64
}

// New constructs a Service from config and functional options.
//
// Resources not injected via options are created from cfg, wrapped as
// lifecycle.Stoppers, and registered with the internal Manager. Stop will
// stop them in reverse order. Injected resources are NOT registered — caller
// owns their lifecycle.
//
// On partial failure (any resolve returns an error), already-registered
// components are stopped via mgr.Stop() before returning the error.
func New(cfg *config.Config, opts ...option.Option) (*Service, error) {
	o := option.Apply(opts...)
	mgr := lifecycle.NewManager()

	db, err := resolveDB(&o, cfg, mgr)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("rollback: %w", cerr))
		}
		return nil, err
	}

	rdb, err := resolveRedis(&o, cfg, mgr)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("rollback: %w", cerr))
		}
		return nil, err
	}

	signer, err := resolveSigner(cfg)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("rollback: %w", cerr))
		}
		return nil, err
	}

	trialDays := int32(14)
	if cfg.Trial != nil && cfg.Trial.Days > 0 {
		trialDays = cfg.Trial.Days
	}

	// jobs.Scheduler owns the cron instance; setupJobs builds it, registers
	// it on mgr, and wires periodic jobs (empty by default — add jobs inside
	// setupJobs as scheduler.AddFunc calls). See architecture.md (jobs.md).
	svc := &Service{
		cfg: cfg,
		mgr: mgr,
		db:  db,
		rdb: rdb,

		activation: activation.New(db, rdb, signer, trialDays),

		startedAt: time.Now().UnixMilli(),
	}

	if err := svc.setupJobs(); err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("rollback: %w", cerr))
		}
		return nil, err
	}

	return svc, nil
}

// Start starts all owned components concurrently.
func (s *Service) Start() error { return s.mgr.Start() }

// Stop stops all owned components in reverse registration order.
func (s *Service) Stop() error { return s.mgr.Stop() }

// Ping is a health-check RPC, always generated so the grpc-gateway has at
// least one HTTP endpoint and pkg/server.go can always register the handler.
// Returns only public, non-sensitive info — never internal addresses, env,
// secrets, or dependency topology.
func (s *Service) Ping(_ context.Context) (*licensev1.Pong, error) {
	v := version.Get()
	return &licensev1.Pong{
		Service:   "license-service",
		Version:   v.Version,
		GitCommit: v.GitCommit,
		GitBranch: v.GitBranch,
		BuildTime: v.BuildTime,
		GoVersion: v.GoVersion,
		Status:    "SERVING",
		Now:       time.Now().UnixMilli(),
		StartedAt: s.startedAt,
	}, nil
}

// --- facade methods (one per RPC, delegate to subpackage) ---

// Activate delegates to the activation domain (A1–A12 converger).
func (s *Service) Activate(ctx context.Context, req *licensev1.ActivateRequest) (*licensev1.ActivateResponse, error) {
	return s.activation.Activate(ctx, req)
}

// Deactivate delegates to the activation domain (idempotent slot release).
func (s *Service) Deactivate(ctx context.Context, req *licensev1.DeactivateRequest) (*licensev1.DeactivateResponse, error) {
	return s.activation.Deactivate(ctx, req)
}

// TrialStart delegates to the activation domain (keyless trial ledger).
func (s *Service) TrialStart(ctx context.Context, req *licensev1.TrialStartRequest) (*licensev1.TrialStartResponse, error) {
	return s.activation.TrialStart(ctx, req)
}

// Resource resolve helpers (resolveDB / resolveRedis)
// live in helper.go — extracted from this file to keep service.go focused on
// the Service struct, New/Start/Stop/Ping, and the facade delegations.

// setupJobs builds the jobs.Scheduler, registers it on s.mgr, and wires
// periodic jobs. Signature is intentionally receiver-only: future jobs are
// added inside this method as scheduler.AddFunc calls. Timezone default lives
// in config.CronConfig's default tag.
func (s *Service) setupJobs() error {
	scheduler, err := jobs.New(&jobs.Deps{
		Config: &cronx.Config{
			Timezone:      s.cfg.Cron.Timezone,
			OverlapPolicy: "skip",
		},
	})
	if err != nil {
		return fmt.Errorf("init jobs: %w", err)
	}
	s.mgr.Add("jobs", scheduler)
	return nil
}
