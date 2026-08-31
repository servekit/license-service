package cert

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// RFC 8032 Ed25519 test vector 1 keys — public test material, pinned by the
// client's cert.rs test module. Ed25519 is deterministic, so both sides must
// produce the identical signature byte-for-byte.
const (
	goldenSeedHex     = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
	goldenPubkeyHex   = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"
	goldenDevice      = "6f9619ff-8b86-d011-b42d-00cf4fc964ff"
	goldenFingerprint = "v1.9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	goldenIssuedAt    = "2026-08-24T12:00:00Z"
	goldenLicenseID   = "lk_9f86d081884c7d659a2feaa0c55ad015"
)

func goldenTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return ts
}

// TestGolden_MainVector: the spec's primary golden cert (design doc §12.1).
// Locks canonical ordering (incl. entitlements key order), escaping, the
// omitted-when-null signingKeyId member, second-precision issuedAt, and the
// deterministic signature.
func TestGolden_MainVector(t *testing.T) {
	wantPayload := `{"certId":"3fa85f64-5717-4562-b3fc-2c963f66afa6","deviceToken":"6f9619ff-8b86-d011-b42d-00cf4fc964ff","entitlements":{"downloads":{"expiresAt":null,"kind":"perpetual"},"tools":{"expiresAt":"2027-08-24T12:00:00Z","kind":"subscription"}},"fingerprintId":"v1.9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","issuedAt":"2026-08-24T12:00:00Z","licenseId":"lk_9f86d081884c7d659a2feaa0c55ad015","signingKeyId":"k1","v":1}`
	wantSig := "Bcz8PToiBjVfHV8ekGIC9bRBWb0YD1+kHGoCPZvNnPuqFqDcQ9YPY9JuoUl9l8391TV1z0WUGFsJEIalBOdzBw=="

	licenseID := goldenLicenseID
	p := &Payload{
		V:             1,
		CertID:        "3fa85f64-5717-4562-b3fc-2c963f66afa6",
		LicenseID:     &licenseID,
		DeviceToken:   goldenDevice,
		FingerprintID: goldenFingerprint,
		IssuedAt:      goldenTime(t, goldenIssuedAt),
		SigningKeyID:  "k1",
		Entitlements: map[string]Entitlement{
			"downloads": {Kind: KindPerpetual},
			"tools": {
				Kind:      KindSubscription,
				ExpiresAt: ptrTime(goldenTime(t, "2027-08-24T12:00:00Z")),
			},
		},
	}
	require.Equal(t, wantPayload, string(p.MarshalCanonical()))

	signer, err := NewSigner(map[string]string{"k1": goldenSeedHex}, "")
	require.NoError(t, err)
	payload, sig, err := signer.Sign(p)
	require.NoError(t, err)
	require.Equal(t, wantPayload, payload)
	require.Equal(t, wantSig, sig)

	pub, err := hex.DecodeString(goldenPubkeyHex)
	require.NoError(t, err)
	require.True(t, Verify(pub, payload, sig))
}

// TestGolden_KeylessVector: keyless trial cert — licenseId null, entitlements
// carry the full trial ledger including an already-expired entry.
func TestGolden_KeylessVector(t *testing.T) {
	wantPayload := `{"certId":"0b8f2c1a-4d3e-4f5a-9b6c-7d8e9f0a1b2c","deviceToken":"6f9619ff-8b86-d011-b42d-00cf4fc964ff","entitlements":{"downloads":{"expiresAt":"2026-09-07T12:00:00Z","kind":"trial"},"tools":{"expiresAt":"2026-08-20T12:00:00Z","kind":"trial"}},"fingerprintId":"v1.9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","issuedAt":"2026-08-24T12:00:00Z","licenseId":null,"signingKeyId":"k1","v":1}`
	wantSig := "rUa0u/lqedL10jj8ZQXUTHdJSe9VSadD9jKG8GmYVmK1Y9teBLGYoVnQzfFvfaUZV8PiIIDk9lDmFu8MmU/OAg=="

	p := &Payload{
		V:             1,
		CertID:        "0b8f2c1a-4d3e-4f5a-9b6c-7d8e9f0a1b2c",
		LicenseID:     nil,
		DeviceToken:   goldenDevice,
		FingerprintID: goldenFingerprint,
		IssuedAt:      goldenTime(t, goldenIssuedAt),
		SigningKeyID:  "k1",
		Entitlements: map[string]Entitlement{
			"downloads": {
				Kind:      KindTrial,
				ExpiresAt: ptrTime(goldenTime(t, "2026-09-07T12:00:00Z")),
			},
			"tools": {
				Kind:      KindTrial,
				ExpiresAt: ptrTime(goldenTime(t, "2026-08-20T12:00:00Z")), // expired entries stay in
			},
		},
	}
	require.Equal(t, wantPayload, string(p.MarshalCanonical()))

	signer, err := NewSigner(map[string]string{"k1": goldenSeedHex}, "")
	require.NoError(t, err)
	payload, sig, err := signer.Sign(p)
	require.NoError(t, err)
	require.Equal(t, wantPayload, payload)
	require.Equal(t, wantSig, sig)
}

// TestGolden_SingleModuleVector: minimal single-module cert with every
// nullable member non-null-able — the shape most sensitive to key-sorting
// omissions (a single entitlement exposes entitlements-object ordering with
// no second member to mask it).
func TestGolden_SingleModuleVector(t *testing.T) {
	wantPayload := `{"certId":"3fa85f64-5717-4562-b3fc-2c963f66afa6","deviceToken":"6f9619ff-8b86-d011-b42d-00cf4fc964ff","entitlements":{"tools":{"expiresAt":"2027-08-24T12:00:00Z","kind":"subscription"}},"fingerprintId":"v1.9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","issuedAt":"2026-08-24T12:00:00Z","licenseId":"lk_9f86d081884c7d659a2feaa0c55ad015","signingKeyId":"k1","v":1}`
	wantSig := "xl6IHxOnXqdRarrtcZJYn5NllLFMLedAB40iAQQHzAG/tUuEQsS/Q7tA957VepnR1cZAyptSB1Yh0WyrgNniAg=="

	licenseID := goldenLicenseID
	p := &Payload{
		V:             1,
		CertID:        "3fa85f64-5717-4562-b3fc-2c963f66afa6",
		LicenseID:     &licenseID,
		DeviceToken:   goldenDevice,
		FingerprintID: goldenFingerprint,
		IssuedAt:      goldenTime(t, goldenIssuedAt),
		SigningKeyID:  "k1",
		Entitlements: map[string]Entitlement{
			"tools": {
				Kind:      KindSubscription,
				ExpiresAt: ptrTime(goldenTime(t, "2027-08-24T12:00:00Z")),
			},
		},
	}
	require.Equal(t, wantPayload, string(p.MarshalCanonical()))

	signer, err := NewSigner(map[string]string{"k1": goldenSeedHex}, "")
	require.NoError(t, err)
	payload, sig, err := signer.Sign(p)
	require.NoError(t, err)
	require.Equal(t, wantPayload, payload)
	require.Equal(t, wantSig, sig)
}

// TestGolden_SigningKeyID: a named-key payload places signingKeyId between
// licenseId and v, and Sign refuses unknown/unconfigured kids (fail-closed,
// no downgrade to the default key).
func TestGolden_SigningKeyID(t *testing.T) {
	licenseID := goldenLicenseID
	kid := "k2027"
	p := &Payload{
		V:             1,
		CertID:        "3fa85f64-5717-4562-b3fc-2c963f66afa6",
		LicenseID:     &licenseID,
		DeviceToken:   goldenDevice,
		FingerprintID: goldenFingerprint,
		IssuedAt:      goldenTime(t, goldenIssuedAt),
		SigningKeyID:  kid,
		Entitlements:  map[string]Entitlement{"tools": {Kind: KindPerpetual}},
	}
	out := string(p.MarshalCanonical())
	require.Contains(t, out, `"licenseId":"`+goldenLicenseID+`","signingKeyId":"k2027","v":1`)

	// Kid not configured on this signer -> fail-closed.
	onlyK1, err := NewSigner(map[string]string{"k1": goldenSeedHex}, "")
	require.NoError(t, err)
	_, _, err = onlyK1.Sign(p)
	require.Error(t, err)

	// Configured -> signs; single key becomes active implicitly.
	s, err := NewSigner(map[string]string{kid: goldenSeedHex}, "")
	require.NoError(t, err)
	payload, sig, err := s.Sign(p)
	require.NoError(t, err)
	require.Equal(t, out, payload)
	require.NotEqual(t, "", sig)
	require.Equal(t, kid, s.ActiveKeyID())
}

// TestSigner_MultiKeyAndActive: several named keys coexist; a dangling
// active kid is a construction error; an active kid signs new payloads via
// the payload stamp, and Sign stays literal/fail-closed.
func TestSigner_MultiKeyAndActive(t *testing.T) {
	seed2 := "4ccd089b28ff96da9db6c346ec114e0f5b8058f5e8ad5b7e2b4b1e7c5d3f5a6b"
	s, err := NewSigner(map[string]string{
		"k2027": seed2,
		"k2028": goldenSeedHex,
	}, "k2027")
	require.NoError(t, err)
	require.Equal(t, "k2027", s.ActiveKeyID())
	require.Len(t, s.PublicKeys(), 2)

	// Dangling active kid is rejected at construction.
	_, err = NewSigner(map[string]string{"k2027": goldenSeedHex}, "k9999")
	require.Error(t, err)

	// Multiple keys without sign_key_id are ambiguous.
	_, err = NewSigner(map[string]string{"k2027": goldenSeedHex, "k2028": seed2}, "")
	require.Error(t, err)

	// Empty kid / empty key set are rejected.
	_, err = NewSigner(map[string]string{"": goldenSeedHex}, "")
	require.Error(t, err)
	_, err = NewSigner(nil, "")
	require.Error(t, err)

	// Sign stays literal: payload kid -> that key's signature.
	licenseID := goldenLicenseID
	p := &Payload{
		V: 1, CertID: "c", DeviceToken: "d", FingerprintID: "f",
		IssuedAt: goldenTime(t, goldenIssuedAt), LicenseID: &licenseID,
		SigningKeyID: "k2028", Entitlements: map[string]Entitlement{},
	}
	payload, sig, err := s.Sign(p)
	require.NoError(t, err)
	require.Contains(t, payload, `"signingKeyId":"k2028"`)
	pub, _ := hex.DecodeString(goldenPubkeyHex)
	require.True(t, Verify(ed25519.PublicKey(pub), payload, sig), "k2028 uses the golden seed")

	// A different key's pubkey must NOT verify it.
	p.SigningKeyID = "k2027"
	payload, sig, err = s.Sign(p)
	require.NoError(t, err)
	otherPub, err := base64.StdEncoding.DecodeString(s.PublicKeys()["k2027"])
	require.NoError(t, err)
	require.True(t, Verify(ed25519.PublicKey(otherPub), payload, sig))
	require.False(t, Verify(ed25519.PublicKey(pub), payload, sig), "distinct seeds must not cross-verify")

	// Unconfigured kid -> fail-closed error.
	p.SigningKeyID = "k9999"
	_, _, err = s.Sign(p)
	require.Error(t, err)
}

func ptrTime(t time.Time) *time.Time { return &t }
