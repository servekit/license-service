package xcodes

import (
	"google.golang.org/protobuf/proto"

	"github.com/servekit/go-common/xerr"
)

// Detailed carries business error details (SlotLimitInfo, RetryAfterInfo, …)
// alongside an xerr error. The error interceptor promotes Details into gRPC
// status details so the future HTTP gateway can flatten them into the error
// body (maxSlots/devices, Retry-After).
type Detailed struct {
	Err     *xerr.Error
	Details []proto.Message
}

// WithDetails wraps e with proto details to be attached to the gRPC status.
func WithDetails(e *xerr.Error, details ...proto.Message) error {
	return &Detailed{Err: e, Details: details}
}

// Error implements error by delegating to the wrapped xerr ("REASON: message").
func (d *Detailed) Error() string { return d.Err.Error() }

// Unwrap exposes the underlying *xerr.Error for errors.As chains.
func (d *Detailed) Unwrap() error { return d.Err }
