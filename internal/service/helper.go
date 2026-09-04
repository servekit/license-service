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

	"github.com/servekit/license-service/pkg/config"

	"github.com/servekit/license-service/internal/service/activation"
	"github.com/servekit/license-service/internal/service/cert"
)

// resolveDomainOptions validates the activation/admin knobs and packs the
// activation Options. Defaults are owned by the config default: tags
// (configx applies them on Load); this only rejects incomplete configs —
// a zero Window/Max would make the limiter deny everything (Max=0) or set
// no TTL (Window=0), so silence would be worse than a startup error.
func resolveDomainOptions(cfg *config.Config) (activation.Options, error) {
	if cfg.Trial == nil || cfg.Trial.Days <= 0 {
		return activation.Options{}, fmt.Errorf("trial.days must be > 0")
	}
	if cfg.RateLimit == nil || cfg.RateLimit.KeyPrefix == "" ||
		cfg.RateLimit.Window <= 0 || cfg.RateLimit.Max <= 0 {
		return activation.Options{}, fmt.Errorf(
			"rate_limit config is required (key_prefix / window / max)")
	}
	return activation.Options{
		TrialDays:     cfg.Trial.Days,
		RateKeyPrefix: cfg.RateLimit.KeyPrefix,
		RateWindow:    cfg.RateLimit.Window,
		RateMax:       cfg.RateLimit.Max,
	}, nil
}

// resolveSigner builds the Ed25519 cert signer from cfg. The signing seed is
// REQUIRED — a license service without key material cannot issue creds, so
// an empty seed is a startup error (fail-fast beats signing with nothing).
func resolveSigner(cfg *config.Config) (*cert.Signer, error) {
	sc := cfg.Signing
	if sc == nil || len(sc.Keys) == 0 {
		return nil, fmt.Errorf("signing.keys needs at least one entry")
	}
	keys := map[string]string{}
	for _, k := range sc.Keys {
		if k == nil {
			continue
		}
		if _, dup := keys[k.KeyID]; dup {
			return nil, fmt.Errorf("duplicate signing key_id %q", k.KeyID)
		}
		keys[k.KeyID] = k.Seed
	}
	signer, err := cert.NewSigner(keys, sc.SignKeyID)
	if err != nil {
		return nil, fmt.Errorf("signer: %w", err)
	}
	return signer, nil
}
