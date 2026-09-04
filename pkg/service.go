package pkg

import (
	licensev1 "github.com/servekit/license-service/gen/license/v1"
)

// Service is how a consumer holds license-service regardless of backend: the
// in-process *Handler (module mode) and the gRPC *Client both satisfy it. It
// embeds both generated server interfaces (main + admin) so the method set
// tracks the proto automatically — no hand-maintained method list here.
type Service interface {
	licensev1.LicenseServiceServer
	licensev1.LicenseAdminServiceServer
}
