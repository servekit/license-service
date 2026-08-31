package cert

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCanonical_RoundTripStable: marshal → unmarshal → marshal is
// byte-stable across a randomized corpus (the client re-derives canonical
// bytes from parsed values; any instability would break verification).
func TestCanonical_RoundTripStable(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		p := randomPayload(rng)
		first := p.MarshalCanonical()
		parsed, err := UnmarshalCanonical(first)
		require.NoError(t, err, "corpus %d: %s", i, first)
		second := parsed.MarshalCanonical()
		require.True(t, bytes.Equal(first, second),
			"corpus %d unstable:\nfirst:  %s\nsecond: %s", i, first, second)
	}
}

// TestCanonical_Escaping: control characters get lowercase-hex \u00xx
// escapes, quotes and backslashes the short forms, and everything else —
// including multi-byte UTF-8 — passes through verbatim.
func TestCanonical_Escaping(t *testing.T) {
	p := &Payload{
		V:             1,
		CertID:        "a\"b\\c",
		DeviceToken:   "tab\there",
		FingerprintID: "ctl\x01\x1f",
		IssuedAt:      time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		Entitlements: map[string]Entitlement{
			"m": {Kind: "中文-kind"},
		},
	}
	out := string(p.MarshalCanonical())
	require.Contains(t, out, `"certId":"a\"b\\c"`)
	require.Contains(t, out, `"deviceToken":"tab\there"`)
	require.Contains(t, out, `"fingerprintId":"ctl\u0001\u001f"`)
	require.Contains(t, out, `"kind":"中文-kind"`)

	parsed, err := UnmarshalCanonical([]byte(out))
	require.NoError(t, err)
	require.Equal(t, p.CertID, parsed.CertID)
	require.Equal(t, p.DeviceToken, parsed.DeviceToken)
	require.Equal(t, p.FingerprintID, parsed.FingerprintID)
}

// TestCanonical_SecondPrecision: issuedAt truncates sub-second precision so
// the wire form is always second-granular RFC3339 UTC.
func TestCanonical_SecondPrecision(t *testing.T) {
	p := &Payload{
		V:             1,
		CertID:        "c",
		DeviceToken:   "d",
		FingerprintID: "f",
		IssuedAt:      time.Date(2026, 8, 24, 12, 0, 0, 999999999, time.FixedZone("CST", 8*3600)),
		Entitlements:  map[string]Entitlement{},
	}
	require.Contains(t, string(p.MarshalCanonical()), `"issuedAt":"2026-08-24T04:00:00Z"`)
}

func randomPayload(rng *rand.Rand) *Payload {
	p := &Payload{
		V:             1,
		CertID:        fmt.Sprintf("%08x-0000-4000-8000-%012x", rng.Uint32(), rng.Uint64()),
		DeviceToken:   fmt.Sprintf("%08x-0000-4000-8000-%012x", rng.Uint32(), rng.Uint64()),
		FingerprintID: "v1." + fmt.Sprintf("%064x", rng.Uint64()),
		IssuedAt:      time.Unix(rng.Int63n(4102444800), 0).UTC(),
		Entitlements:  map[string]Entitlement{},
	}
	if rng.Intn(2) == 0 {
		id := "lk_" + fmt.Sprintf("%032x", rng.Uint64())
		p.LicenseID = &id
	}
	if rng.Intn(4) == 0 {
		p.SigningKeyID = fmt.Sprintf("k%d", rng.Intn(100))
	}
	for _, m := range []string{"downloads", "tools", "zzz"} {
		if rng.Intn(2) == 0 {
			continue
		}
		e := Entitlement{Kind: KindPerpetual}
		if rng.Intn(2) == 0 {
			e.Kind = []Kind{KindSubscription, KindTrial}[rng.Intn(2)]
			exp := time.Unix(rng.Int63n(4102444800), 0).UTC()
			e.ExpiresAt = &exp
		}
		p.Entitlements[m] = e
	}
	return p
}
