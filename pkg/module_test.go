package pkg_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/stretchr/testify/require"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/redisx"

	"github.com/servekit/license-service/pkg"
	"github.com/servekit/license-service/pkg/config"
	"github.com/servekit/license-service/pkg/option"
)

// TestNewModule_Ping: the in-process module entry constructs from config
// with injected resources (caller owns lifecycle) and answers RPCs.
func TestNewModule_Ping(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	rdb := redisx.NewTestClient(t)

	hdl, err := pkg.NewModule(&config.Config{
		Server:  &config.ServerConfig{},
		Signing: &config.SigningConfig{Seed: "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"},
		Trial:   &config.TrialConfig{Days: 14},
		// Hand-built configs get no default-tag treatment (that is
		// configx's job on Load) — every knob must be present.
		RateLimit: &config.RateLimitConfig{KeyPrefix: "license:rate", Window: time.Minute, Max: 10},
	},
		option.WithDB(db),
		option.WithRedis(rdb),
	)
	require.NoError(t, err)

	pong, err := hdl.Ping(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	require.Equal(t, "license-service", pong.GetService())
	require.Equal(t, "SERVING", pong.GetStatus())

	// Caller-owned resources stay owned by the caller (module never closes them).
	require.NotNil(t, db)
	require.NotNil(t, rdb)
}
