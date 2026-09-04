package pkg

import (
	"fmt"

	"github.com/servekit/go-common/configx"
	"github.com/servekit/go-common/lifecycle"

	"github.com/servekit/license-service/pkg/config"
	"github.com/servekit/license-service/pkg/option"
)

// moduleClaim enforces one live module instance per process for Connect.
var moduleClaim lifecycle.ModuleClaim

// ConnectConfig describes how to connect to license-service. Mode selects the
// backend: "grpc" dials Target with the server-shaped *Client, "module" (the
// default when empty) builds an in-process Handler from Config. Opts carries
// resource injection for module mode — shared db/redis via WithDB/WithRedis.
type ConnectConfig struct {
	Mode   configx.Mode    // "grpc" | "module" ("" = module)
	Target string          // grpc dial target; required when Mode=grpc
	Config *config.Config  // module-mode config; required when Mode=module
	Opts   []option.Option // module-mode resource injection (WithDB/WithRedis)
}

// Connect resolves a license-service dependency end to end and registers its
// lifecycle with mgr: grpc mode registers a Stopper (closes the connection);
// module mode registers the raw Handler via mgr.Add so the consumer drives
// its Start/Stop. It does NOT handle a parent-injected Handler — adoption is
// the consumer's call (return the injected value and skip Connect), because
// it reads the consumer's own options and the parent owns that lifecycle.
//
// The returned *Handler is non-nil only in module mode, so an embedding
// composition can share this instance downstream.
func Connect(cfg ConnectConfig, mgr *lifecycle.Manager) (Service, *Handler, error) {
	switch cfg.Mode {
	case configx.ModeGRPC:
		if cfg.Target == "" {
			return nil, nil, fmt.Errorf("license-service: target required when mode=grpc")
		}
		c, err := NewClient(cfg.Target)
		if err != nil {
			return nil, nil, fmt.Errorf("license-service: %w", err)
		}
		mgr.AddStopper("license-service", lifecycle.StopFunc(func() { _ = c.Close() }))
		return c, nil, nil
	case configx.ModeModule, configx.ModeUnspecified:
		if cfg.Config == nil {
			return nil, nil, fmt.Errorf("license-service: module config required")
		}
		if err := moduleClaim.Claim("license-service"); err != nil {
			return nil, nil, err
		}
		hdl, err := NewModule(cfg.Config, cfg.Opts...)
		if err != nil {
			moduleClaim.Release() // construction failed; free the slot
			return nil, nil, fmt.Errorf("license-service: %w", err)
		}
		mgr.Add("license-service", moduleClaim.Wrap(hdl))
		return hdl, hdl, nil
	default:
		return nil, nil, fmt.Errorf("license-service: unknown mode %q (want \"grpc\" or \"module\")", cfg.Mode)
	}
}
