package cert

import (
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
	wantPayload := `{"certId":"3fa85f64-5717-4562-b3fc-2c963f66afa6","deviceToken":"6f9619ff-8b86-d011-b42d-00cf4fc964ff","entitlements":{"downloads":{"expiresAt":null,"kind":"perpetual"},"tools":{"expiresAt":"2027-08-24T12:00:00Z","kind":"subscription"}},"fingerprintId":"v1.9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","issuedAt":"2026-08-24T12:00:00Z","licenseId":"lk_9f86d081884c7d659a2feaa0c55ad015","v":1}`
	wantSig := "VBDWouHSgIv5DbSYiQC880OpDk9gjuZMlpcIcLVDAq0G0Vz7iOexr2hApkTL2aiTEqzcjjtZacrw82hT0IaoDQ=="

	licenseID := goldenLicenseID
	p := &Payload{
		V:             1,
		CertID:        "3fa85f64-5717-4562-b3fc-2c963f66afa6",
		LicenseID:     &licenseID,
		DeviceToken:   goldenDevice,
		FingerprintID: goldenFingerprint,
		IssuedAt:      goldenTime(t, goldenIssuedAt),
		Entitlements: map[string]Entitlement{
			"downloads": {Kind: KindPerpetual},
			"tools": {
				Kind:      KindSubscription,
				ExpiresAt: ptrTime(goldenTime(t, "2027-08-24T12:00:00Z")),
			},
		},
	}
	require.Equal(t, wantPayload, string(p.MarshalCanonical()))

	signer, err := NewSigner(goldenSeedHex, "", "")
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
	wantPayload := `{"certId":"0b8f2c1a-4d3e-4f5a-9b6c-7d8e9f0a1b2c","deviceToken":"6f9619ff-8b86-d011-b42d-00cf4fc964ff","entitlements":{"downloads":{"expiresAt":"2026-09-07T12:00:00Z","kind":"trial"},"tools":{"expiresAt":"2026-08-20T12:00:00Z","kind":"trial"}},"fingerprintId":"v1.9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","issuedAt":"2026-08-24T12:00:00Z","licenseId":null,"v":1}`
	wantSig := "C0hWnqzldco1YDrB+WlrijMyyXhv35hw9f9QFzFegbEzpPlwQOfbZ8w9m9F/RcPzUQpXqZWEDTAjkJwcKKcxDg=="

	p := &Payload{
		V:             1,
		CertID:        "0b8f2c1a-4d3e-4f5a-9b6c-7d8e9f0a1b2c",
		LicenseID:     nil,
		DeviceToken:   goldenDevice,
		FingerprintID: goldenFingerprint,
		IssuedAt:      goldenTime(t, goldenIssuedAt),
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

	signer, err := NewSigner(goldenSeedHex, "", "")
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
	wantPayload := `{"certId":"3fa85f64-5717-4562-b3fc-2c963f66afa6","deviceToken":"6f9619ff-8b86-d011-b42d-00cf4fc964ff","entitlements":{"tools":{"expiresAt":"2027-08-24T12:00:00Z","kind":"subscription"}},"fingerprintId":"v1.9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","issuedAt":"2026-08-24T12:00:00Z","licenseId":"lk_9f86d081884c7d659a2feaa0c55ad015","v":1}`
	wantSig := "bcHKQJWvbb9VnRg5JzpkgQ50dVugLedwPLJHjtkcStgRCTKzhcHijHm8GrryzsPFb/JaJycTseGiD86ellGOCg=="

	licenseID := goldenLicenseID
	p := &Payload{
		V:             1,
		CertID:        "3fa85f64-5717-4562-b3fc-2c963f66afa6",
		LicenseID:     &licenseID,
		DeviceToken:   goldenDevice,
		FingerprintID: goldenFingerprint,
		IssuedAt:      goldenTime(t, goldenIssuedAt),
		Entitlements: map[string]Entitlement{
			"tools": {
				Kind:      KindSubscription,
				ExpiresAt: ptrTime(goldenTime(t, "2027-08-24T12:00:00Z")),
			},
		},
	}
	require.Equal(t, wantPayload, string(p.MarshalCanonical()))

	signer, err := NewSigner(goldenSeedHex, "", "")
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
		SigningKeyID:  &kid,
		Entitlements:  map[string]Entitlement{"tools": {Kind: KindPerpetual}},
	}
	out := string(p.MarshalCanonical())
	require.Contains(t, out, `"licenseId":"`+goldenLicenseID+`","signingKeyId":"k2027","v":1`)

	// No secondary configured -> signing with a named kid must fail.
	defaultOnly, err := NewSigner(goldenSeedHex, "", "")
	require.NoError(t, err)
	_, _, err = defaultOnly.Sign(p)
	require.Error(t, err)

	// With the named secondary configured, it signs and verifies.
	rotated, err := NewSigner(goldenSeedHex, goldenSeedHex, kid)
	require.NoError(t, err)
	payload, sig, err := rotated.Sign(p)
	require.NoError(t, err)
	require.Equal(t, out, payload)
	require.NotEqual(t, "", sig)
	require.Equal(t, rotated.SecondaryPublicKeyB64(), rotated.PublicKeyB64())
}

func ptrTime(t time.Time) *time.Time { return &t }
