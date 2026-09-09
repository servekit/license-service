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

	commonv1 "github.com/servekit/api/gen/go/common/v1"
	licensev1 "github.com/servekit/api/gen/go/license/v1"
	"github.com/servekit/license-service/internal/jobs"
	"github.com/servekit/license-service/internal/service/activation"
	"github.com/servekit/license-service/internal/service/admin"
	"github.com/servekit/license-service/internal/service/health"
	"github.com/servekit/license-service/internal/version"

	"github.com/servekit/license-service/pkg/config"
	"github.com/servekit/license-service/pkg/option"

	"github.com/servekit/go-common/cronx"
	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/lifecycle"
	"github.com/servekit/go-common/redisx"
	"google.golang.org/protobuf/types/known/emptypb"
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
	// admin owns the operator surface (keys, grants, devices, trials).
	admin *admin.Service
	// health probes DB and signing readiness.
	health *health.Service

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

	db, err := dbx.Connect(cfg.Database, o.DB, mgr)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("rollback: %w", cerr))
		}
		return nil, err
	}

	rdb, err := redisx.Connect(cfg.Redis, o.Redis, mgr)
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

	actOpts, err := resolveDomainOptions(cfg)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("rollback: %w", cerr))
		}
		return nil, err
	}
	trialDays := actOpts.TrialDays

	// jobs.Scheduler owns the cron instance; setupJobs builds it, registers
	// it on mgr, and wires periodic jobs (empty by default — add jobs inside
	// setupJobs as scheduler.AddFunc calls). See architecture.md (jobs.md).
	svc := &Service{
		cfg: cfg,
		mgr: mgr,
		db:  db,
		rdb: rdb,

		activation: activation.New(db, rdb, signer, actOpts),
		admin:      admin.New(db, signer, trialDays),
		health:     health.New(db, signer),

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
func (s *Service) Ping(_ context.Context) (*commonv1.Pong, error) {
	v := version.Get()
	return &commonv1.Pong{
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

// Activate delegates to the activation domain (A1–A12 converger). The
// calling app must present valid credentials (requireApp — fail closed).
func (s *Service) Activate(ctx context.Context, req *licensev1.ActivateRequest) (*licensev1.ActivateResponse, error) {
	if _, err := s.requireApp(ctx); err != nil {
		return nil, err
	}
	return s.activation.Activate(ctx, req)
}

// Deactivate delegates to the activation domain (idempotent slot release).
// The calling app must present valid credentials (requireApp — fail closed).
func (s *Service) Deactivate(ctx context.Context, req *licensev1.DeactivateRequest) (*licensev1.DeactivateResponse, error) {
	if _, err := s.requireApp(ctx); err != nil {
		return nil, err
	}
	return s.activation.Deactivate(ctx, req)
}

// TrialStart delegates to the activation domain (keyless trial ledger).
// The calling app must present valid credentials (requireApp — fail closed).
func (s *Service) TrialStart(ctx context.Context, req *licensev1.TrialStartRequest) (*licensev1.TrialStartResponse, error) {
	if _, err := s.requireApp(ctx); err != nil {
		return nil, err
	}
	return s.activation.TrialStart(ctx, req)
}

// Health reports DB and signing readiness; any failure is Unavailable (503).
func (s *Service) Health(ctx context.Context, req *licensev1.HealthRequest) (*licensev1.HealthResponse, error) {
	return s.health.Health(ctx, req)
}

// --- admin facades (one per LicenseAdminService RPC) ---

// CreateKey mints a key; the plaintext appears exactly once in the response.
func (s *Service) CreateKey(ctx context.Context, req *licensev1.CreateKeyRequest) (*licensev1.CreateKeyResponse, error) {
	return s.admin.CreateKey(ctx, req)
}

// ShowKey returns the full key view with the slot roster.
func (s *Service) ShowKey(ctx context.Context, req *licensev1.ShowKeyRequest) (*licensev1.ShowKeyResponse, error) {
	return s.admin.ShowKey(ctx, req)
}

// ListKeys returns the key roster (status 0 = all).
func (s *Service) ListKeys(ctx context.Context, req *licensev1.ListKeysRequest) (*licensev1.ListKeysResponse, error) {
	return s.admin.ListKeys(ctx, req)
}

// UpdateKey applies optional label/slots updates.
func (s *Service) UpdateKey(ctx context.Context, req *licensev1.UpdateKeyRequest) (*licensev1.UpdateKeyResponse, error) {
	return s.admin.UpdateKey(ctx, req)
}

// RevokeKey freezes the key soft-state.
func (s *Service) RevokeKey(ctx context.Context, req *licensev1.RevokeKeyRequest) (*licensev1.RevokeKeyResponse, error) {
	return s.admin.RevokeKey(ctx, req)
}

// UnrevokeKey reactivates a revoked key.
func (s *Service) UnrevokeKey(ctx context.Context, req *licensev1.UnrevokeKeyRequest) (*licensev1.UnrevokeKeyResponse, error) {
	return s.admin.UnrevokeKey(ctx, req)
}

// DeleteKey physically deletes the key with app-level cascade.
func (s *Service) DeleteKey(ctx context.Context, req *licensev1.DeleteKeyRequest) (*licensev1.DeleteKeyResponse, error) {
	return s.admin.DeleteKey(ctx, req)
}

// GrantModule upserts one module entitlement.
func (s *Service) GrantModule(ctx context.Context, req *licensev1.GrantModuleRequest) (*licensev1.GrantModuleResponse, error) {
	return s.admin.GrantModule(ctx, req)
}

// RevokeModule removes one module entitlement.
func (s *Service) RevokeModule(ctx context.Context, req *licensev1.RevokeModuleRequest) (*licensev1.RevokeModuleResponse, error) {
	return s.admin.RevokeModule(ctx, req)
}

// ListKeyDevices returns the slot roster of a key.
func (s *Service) ListKeyDevices(ctx context.Context, req *licensev1.ListKeyDevicesRequest) (*licensev1.ListKeyDevicesResponse, error) {
	return s.admin.ListKeyDevices(ctx, req)
}

// KickDevice force-releases one slot (support-side eviction).
func (s *Service) KickDevice(ctx context.Context, req *licensev1.KickDeviceRequest) (*licensev1.KickDeviceResponse, error) {
	return s.admin.KickDevice(ctx, req)
}

// ShowTrial returns a fingerprint's trial ledger.
func (s *Service) ShowTrial(ctx context.Context, req *licensev1.ShowTrialRequest) (*licensev1.ShowTrialResponse, error) {
	return s.admin.ShowTrial(ctx, req)
}

// ResetTrial deletes one trial ledger row (manual reset channel).
func (s *Service) ResetTrial(ctx context.Context, req *licensev1.ResetTrialRequest) (*licensev1.ResetTrialResponse, error) {
	return s.admin.ResetTrial(ctx, req)
}

// ShowPubKey exposes the signing public key(s) for client pinning.
func (s *Service) ShowPubKey(ctx context.Context, req *licensev1.ShowPubKeyRequest) (*licensev1.ShowPubKeyResponse, error) {
	return s.admin.ShowPubKey(ctx, req)
}

// --- app-registry facades (calling applications; message-service pattern) ---

// CreateApp registers a calling app; the minted secret rides the response.
func (s *Service) CreateApp(ctx context.Context, req *licensev1.CreateAppRequest) (*licensev1.CreateAppResponse, error) {
	return s.admin.CreateApp(ctx, req)
}

// GetApp returns one app by app_key.
func (s *Service) GetApp(ctx context.Context, req *licensev1.GetAppRequest) (*licensev1.GetAppResponse, error) {
	return s.admin.GetApp(ctx, req)
}

// UpdateApp edits mutable fields (name, disabled); app_key is immutable.
func (s *Service) UpdateApp(ctx context.Context, req *licensev1.UpdateAppRequest) (*licensev1.UpdateAppResponse, error) {
	return s.admin.UpdateApp(ctx, req)
}

// RotateAppSecret mints a new app secret.
func (s *Service) RotateAppSecret(ctx context.Context, req *licensev1.RotateAppSecretRequest) (*licensev1.RotateAppSecretResponse, error) {
	return s.admin.RotateAppSecret(ctx, req)
}

// ListApps lists all calling apps (no paging — low cardinality).
func (s *Service) ListApps(ctx context.Context, req *licensev1.ListAppsRequest) (*licensev1.ListAppsResponse, error) {
	return s.admin.ListApps(ctx, req)
}

// DeleteApp removes the app row (hard delete).
func (s *Service) DeleteApp(ctx context.Context, req *licensev1.DeleteAppRequest) (*emptypb.Empty, error) {
	return s.admin.DeleteApp(ctx, req)
}

// Resource resolve helpers (resolveDB / resolveRedis)
// live in helper.go — extracted from this file to keep service.go focused on
// the Service struct, New/Start/Stop/Ping, and the facade delegations.

// setupJobs builds the jobs.Scheduler, registers it on s.mgr, and wires
// periodic jobs. Signature is intentionally receiver-only: future jobs are
// added inside this method as scheduler.AddFunc calls. Timezone default lives
// in config.CronConfig's default tag.
func (s *Service) setupJobs() error {
	timezone := ""
	if s.cfg.Cron != nil { // configx always allocates; direct constructors may not
		timezone = s.cfg.Cron.Timezone
	}
	scheduler, err := jobs.New(&jobs.Deps{
		Config: &cronx.Config{
			Timezone:      timezone,
			OverlapPolicy: "skip",
		},
	})
	if err != nil {
		return fmt.Errorf("init jobs: %w", err)
	}
	s.mgr.Add("jobs", scheduler)
	return nil
}
