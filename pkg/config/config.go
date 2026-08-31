// Package config defines the license-service configuration shape and loads it
// via go-common's configx.
//
// serviceName and envPrefix are the two anchors external tooling (systemd
// unit, Docker env, k8s configmap) must agree on with this binary.
package config

import (
	"time"

	"github.com/servekit/go-common/configx"
	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/logging"
	"github.com/servekit/go-common/redisx"
)

// serviceName identifies this binary in config file lookup (/etc/<name>) and
// the <NAME>_CONFIG env var. envPrefix scopes all env overrides under
// LICENSE_SERVICE_.
const (
	serviceName = "license-service"
	envPrefix   = "LICENSE_SERVICE"
)

// Config holds all configuration for the license service.
//
// Sub-config fields are pointers per golang-development skill §14: keeps
// style consistent with the outer *Config return + functional options, and
// avoids large-struct copies. Note: configx (viper) ALWAYS allocates nil
// pointer fields during unmarshal, so cfg.X == nil is never true — express
// "optional dependency" via an inner Enabled bool, not pointer nil-check.
type Config struct {
	Server *ServerConfig
	// Database is the primary relational store (wired when --db).
	Database *dbx.Config
	// Redis is the cache/key-value store (wired when --redis).
	Redis *redisx.Config
	// Signing holds the Ed25519 cert-signing key material (design doc §9).
	Signing *SigningConfig
	// AdminToken is the Bearer token required by every LicenseAdminService
	// RPC. Empty = all admin RPCs are rejected (fail-closed).
	AdminToken string
	// Trial configures keyless trial accounting (design doc §7.4).
	Trial *TrialConfig
	// RateLimit configures the per-identity fixed-window limiter
	// (design doc §7.5).
	RateLimit *RateLimitConfig
	Cron      *CronConfig
	Log       *logging.Config
}

// SigningConfig holds the Ed25519 seed material for license cert signing.
// Secrets are injected via environment expansion (${LICENSE_SIGNING_SEED} in
// config.example.yaml); they are never committed, logged, or stored in the DB.
type SigningConfig struct {
	// Seed is the 64-hex primary Ed25519 seed (32 bytes). REQUIRED — service
	// startup fails without it.
	Seed string
	// SeedSecondary is the optional 64-hex named-key seed used during key
	// rotation; it is only used when KeyID is non-empty.
	SeedSecondary string
	// KeyID names the secondary key embedded in cert payloads (signingKeyId).
	// Empty = sign with the default key (signingKeyId null in the payload).
	KeyID string
}

// TrialConfig configures keyless trial accounting.
type TrialConfig struct {
	// Days is the trial duration. Trial expiry is derived server-side as
	// started_at + Days on the server clock; the client never supplies it.
	Days int32 `default:"14"`
}

// RateLimitConfig configures the per-identity fixed-window limiter
// (go-common ratelimit): at most Max requests per Window for each
// (key_hash or fingerprint_id, device_token) pair. The purpose is
// anti-abuse (blocking hammering), deliberately generous for legitimate
// flows — the client heartbeat is 24h and never retries.
type RateLimitConfig struct {
	// KeyPrefix is the Redis key prefix for limiter keys (go-common
	// convention <module>:<purpose>; the package appends ":<purpose>").
	KeyPrefix string `default:"license:rate"`
	// Window is the fixed-window duration. The Retry-After hint sent to
	// clients derives from it.
	Window time.Duration `default:"60s"`
	// Max is the request quota per window per (identity, device_token).
	Max int64 `default:"10"`
}

// ServerConfig holds gRPC and HTTP server addresses.
type ServerConfig struct {
	// GRPCAddr defaults to ":19096" — the servekit fleet port sequence
	// (gid 19091 … user 19094, testkit 19095).
	GRPCAddr string `default:":19096"`
	// HTTPAddr is the grpc-gateway port; empty disables HTTP. license-service
	// is gRPC-only this iteration — the client-facing HTTP face is served by a
	// future standalone gateway, so the default is empty and registerGW stays
	// nil in pkg/server.go. :18086 is reserved for that gateway.
	HTTPAddr string `default:""`
}

// CronConfig configures the internal cronx instance used by jobs.Scheduler.
// Empty by default — jobs.Scheduler is wired but registers no jobs until
// setupJobs adds them.
type CronConfig struct {
	// Timezone for cron expression evaluation. Defaults to Asia/Shanghai.
	Timezone string `default:"Asia/Shanghai"`
}

// Load reads config from the standard configx locations:
//   - /etc/license-service/config.yaml
//   - ./config.yaml
//   - $LICENSE_SERVICE_CONFIG
//
// config.example.yaml is fully placeholder-driven: every value is a ${VAR}
// reference expanded from the process environment (WithExpandEnv), so the file
// holds structure only — all actual values live in .env.example / the runtime
// env. Env vars under $LICENSE_SERVICE_ also override file values via
// viper's automatic binding; struct `default:` tags apply last.
func Load() (*Config, error) {
	var cfg Config
	if err := configx.Load(&cfg,
		configx.WithServiceName(serviceName),
		configx.WithEnvPrefix(envPrefix),
		// Expand ${VAR} placeholders in config values from the process env.
		configx.WithExpandEnv(),
	); err != nil {
		return nil, err
	}
	return &cfg, nil
}
