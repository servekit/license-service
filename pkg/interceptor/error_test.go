package interceptor_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/stretchr/testify/require"

	licensev1 "github.com/servekit/api/gen/go/license/v1"
	"github.com/servekit/license-service/pkg/interceptor"
	"github.com/servekit/license-service/pkg/xcodes"
)

var info = &grpc.UnaryServerInfo{FullMethod: "/license.v1.LicenseService/Activate"}

// TestError_MapsXerrCategories asserts the client error contract: every
// license xerr category lands on the exact gRPC code and the status message
// keeps the "REASON: message" form.
func TestError_MapsXerrCategories(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		reason string
		want   codes.Code
	}{
		{"bad_key_format", xcodes.ErrBadKeyFormat.New("nope"), "BAD_KEY_FORMAT", codes.InvalidArgument},
		{"key_not_found", xcodes.ErrKeyNotFound.New(), "KEY_NOT_FOUND", codes.Unauthenticated},
		{"key_revoked", xcodes.ErrKeyRevoked.New(), "KEY_REVOKED", codes.PermissionDenied},
		{"slot_limit", xcodes.ErrSlotLimit.New(), "SLOT_LIMIT", codes.AlreadyExists},
		{"already_entitled", xcodes.ErrAlreadyEntitled.New(), "ALREADY_ENTITLED", codes.InvalidArgument},
		{"rate_limited", xcodes.ErrRateLimited.New(), "RATE_LIMITED", codes.ResourceExhausted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := interceptor.Error(context.Background(), nil, info,
				func(context.Context, any) (any, error) { return nil, tc.err })
			require.Error(t, err)

			st := status.Convert(err)
			require.Equal(t, tc.want, st.Code())
			// "REASON: message" form — the future gateway splits on the
			// first ": " to recover the lowercase error code.
			require.True(t, strings.HasPrefix(st.Message(), tc.reason+": "),
				"status message %q must start with %q + \": \"", st.Message(), tc.reason)
		})
	}
}

// TestError_PassesPlainErrorsThrough: non-xerr errors are returned unchanged.
func TestError_PassesPlainErrorsThrough(t *testing.T) {
	sentinel := errors.New("boom")
	_, err := interceptor.Error(context.Background(), nil, info,
		func(context.Context, any) (any, error) { return nil, sentinel })
	require.Equal(t, sentinel, err)
}

// TestError_AttachesDetails: Detailed errors carry proto details through
// status.WithDetails.
func TestError_AttachesDetails(t *testing.T) {
	detail := &licensev1.SlotLimitInfo{
		MaxSlots: 3,
		Devices:  []*licensev1.DeviceSlotInfo{{DeviceToken: "6f9619ff-8b86-d011-b42d-00cf4fc964ff"}},
	}
	_, err := interceptor.Error(context.Background(), nil, info,
		func(context.Context, any) (any, error) {
			return nil, xcodes.WithDetails(xcodes.ErrSlotLimit.New(), detail)
		})
	require.Error(t, err)

	st := status.Convert(err)
	require.Equal(t, codes.AlreadyExists, st.Code())

	var got *licensev1.SlotLimitInfo
	for _, d := range st.Details() {
		if v, ok := d.(*licensev1.SlotLimitInfo); ok {
			got = v
		}
	}
	require.NotNil(t, got, "SlotLimitInfo detail must survive status.WithDetails")
	require.EqualValues(t, 3, got.GetMaxSlots())
	require.Len(t, got.GetDevices(), 1)
}

// TestError_SuccessPassesThrough: nil error returns the handler response.
func TestError_SuccessPassesThrough(t *testing.T) {
	resp := wrapperspb.String("ok")
	got, err := interceptor.Error(context.Background(), nil, info,
		func(context.Context, any) (any, error) { return resp, nil })
	require.NoError(t, err)
	require.Equal(t, resp, got)
}

// TestError_UnwrapsChainedDetailed: fmt.Errorf-wrapped Detailed errors are
// still recognized.
func TestError_UnwrapsChainedDetailed(t *testing.T) {
	_, err := interceptor.Error(context.Background(), nil, info,
		func(context.Context, any) (any, error) {
			return nil, fmt.Errorf("activate: %w",
				xcodes.WithDetails(xcodes.ErrKeyRevoked.New(), &licensev1.RetryAfterInfo{Seconds: 60}))
		})
	require.Error(t, err)
	require.Equal(t, codes.PermissionDenied, status.Convert(err).Code())
}
