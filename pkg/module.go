package pkg

import (
	licensev1 "github.com/servekit/license-service/gen/license/v1"
	"github.com/servekit/license-service/internal/service"
	"github.com/servekit/license-service/pkg/config"
	"github.com/servekit/license-service/pkg/handler"
	"github.com/servekit/license-service/pkg/option"
	"gorm.io/gorm"
)

// Handler is the in-process entry point. Callers invoke proto-typed RPC
// methods directly on it — no serialization, no network. This IS the public
// capability surface of license-service when embedded as a module.
//
// Aliased to *handler.Handler so external code references it as
// licensepkg.Handler without importing internal packages.
type Handler = handler.Handler

// Compile-time assertion: *Handler satisfies the gRPC server interface.
var _ licensev1.LicenseServiceServer = (*Handler)(nil)

// NewModule constructs an in-process license service for embedding.
//
// Returns only the Handler — Handler IS the public capability and ALSO
// satisfies signalx.Service (Start/Stop), so module users manage lifecycle
// via the same object they call RPC methods on:
//
//	hdl, err := licensepkg.NewModule(cfg, option.WithDB(parentDB))
//	if err != nil { panic(err) }
//	if err := hdl.Start(); err != nil { panic(err) }   // background goroutines (cron, etc.)
//	defer hdl.Stop()                                    // closes owned resources
//	license, err := hdl.GetLicense(ctx, &licensev1.GetLicenseRequest{Id: 1})
//
// Resources injected via option.WithDB / WithGIDHandler are NOT owned by the
// service — parent process keeps ownership and is responsible for cleanup.
// Only resources the service creates from cfg are tracked by the internal
// lifecycle.Manager and stopped on Stop.
func NewModule(cfg *config.Config, opts ...option.Option) (*Handler, error) {
	svc, err := service.New(cfg, opts...)
	if err != nil {
		return nil, err
	}
	return handler.New(svc), nil
}

// Migrate applies the current schema (GORM AutoMigrate) to db. It re-exports
// handler.Migrate at the package surface so embedders and the `migrate`
// subcommand share one entry point:
//
//	licensepkg.Migrate(parentDB)                              // before NewModule
//	hdl, err := licensepkg.NewModule(cfg, option.WithDB(parentDB))
func Migrate(db *gorm.DB) error {
	return handler.Migrate(db)
}

