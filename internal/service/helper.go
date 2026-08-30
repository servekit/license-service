package service

// This file holds the resource resolve helpers used by service.New. They were
// extracted from service.go to keep that file focused on the Service struct,
// New/Start/Stop/Ping, and the RPC facade delegations.
//
// Each resolve* returns a resource: an injected one (option.With…) is used
// as-is with the caller owning its lifecycle; otherwise it is built from cfg
// and registered with the lifecycle Manager, which starts and stops it.

import (
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"

	"gorm.io/gorm"

	"github.com/servekit/license-service/pkg/config"
	"github.com/servekit/license-service/pkg/option"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/lifecycle"
	"github.com/servekit/go-common/redisx"

	"github.com/servekit/license-service/internal/service/cert"
)

// resolveDB returns the DB to use. If injected via option.WithDB, ownership
// stays with the caller and nothing is registered with mgr. If created from
// cfg, a Stopper is registered so mgr.Stop closes the connection pool.
func resolveDB(o *option.Options, cfg *config.Config, mgr *lifecycle.Manager) (*gorm.DB, error) {
	if o.DB != nil {
		return o.DB, nil
	}
	db, err := dbx.New(cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	mgr.AddStopper("db", lifecycle.StopFunc(func() {
		if sqlDB, e := db.DB(); e == nil && sqlDB != nil {
			if cerr := sqlDB.Close(); cerr != nil {
				slog.Warn("close db", "error", cerr)
			}
		}
	}))
	return db, nil
}

// resolveRedis returns the Redis client to use. If injected via option, ownership
// stays with the caller. If created from cfg, a Stopper is registered so mgr.Stop
// closes the client.
func resolveRedis(o *option.Options, cfg *config.Config, mgr *lifecycle.Manager) (*redis.Client, error) {
	if o.Redis != nil {
		return o.Redis, nil
	}
	rdb, err := redisx.New(cfg.Redis)
	if err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}
	mgr.AddStopper("redis", lifecycle.StopFunc(func() {
		if cerr := rdb.Close(); cerr != nil {
			slog.Warn("close redis", "error", cerr)
		}
	}))
	return rdb, nil
}

// resolveSigner builds the Ed25519 cert signer from cfg. The signing seed is
// REQUIRED — a license service without key material cannot issue creds, so
// an empty seed is a startup error (fail-fast beats signing with nothing).
func resolveSigner(cfg *config.Config) (*cert.Signer, error) {
	sc := cfg.Signing
	if sc == nil || sc.Seed == "" {
		return nil, fmt.Errorf("signing.seed is required (set LICENSE_SIGNING_SEED)")
	}
	signer, err := cert.NewSigner(sc.Seed, sc.SeedSecondary, sc.KeyID)
	if err != nil {
		return nil, fmt.Errorf("signer: %w", err)
	}
	return signer, nil
}
