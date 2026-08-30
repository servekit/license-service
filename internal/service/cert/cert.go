package cert

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"time"
)

// Kind is the billing model of an entitlement entry, carried as its wire
// name ("perpetual" / "subscription" / "trial"). The proto enum converts to
// this at the service boundary — the cert payload is hand-serialized and
// never goes through protojson.
type Kind string

// Entitlement kinds.
const (
	KindPerpetual    Kind = "perpetual"
	KindSubscription Kind = "subscription"
	KindTrial        Kind = "trial"
)

// Payload is the cert v1 model (design doc §8.2). Invariants enforced by the
// signer's callers (admin grant + activation, both sides validated):
//
//   - perpetual entries have ExpiresAt == nil;
//   - subscription/trial entries always have ExpiresAt != nil.
type Payload struct {
	V             int
	CertID        string
	LicenseID     *string // nil = keyless trial cert
	DeviceToken   string
	FingerprintID string
	IssuedAt      time.Time
	SigningKeyID  *string // nil = default key; non-nil must match Signer.keyID
	Entitlements  map[string]Entitlement
}

// Entitlement is one module entry. Module names are the client contract
// names ("downloads", "tools").
type Entitlement struct {
	Kind      Kind
	ExpiresAt *time.Time
}

// formatTime renders t as RFC3339 UTC at second precision — the wire form of
// issuedAt and entitlement expiry, and the client's offline-tolerance anchor.
func formatTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad RFC3339 time %q: %w", s, err)
	}
	return t, nil
}

// Signer holds the Ed25519 key material: a mandatory default key (signingKeyId
// null) and an optional named secondary key for rotation transitions.
type Signer struct {
	priv      ed25519.PrivateKey
	secondary ed25519.PrivateKey // nil unless rotating
	keyID     string             // names secondary; empty when not rotating
}

// NewSigner builds a Signer from 64-hex seeds. seed is required;
// secondaryHex/keyID configure the rotation transition (new certs signed with
// the named key, old certs keep verifying against the pinned default).
func NewSigner(seedHex, secondaryHex, keyID string) (*Signer, error) {
	priv, err := seedFromHex(seedHex)
	if err != nil {
		return nil, fmt.Errorf("signing seed: %w", err)
	}
	s := &Signer{priv: priv}
	if secondaryHex != "" {
		if keyID == "" {
			return nil, fmt.Errorf("signing key_id is required when a secondary seed is configured")
		}
		sec, err := seedFromHex(secondaryHex)
		if err != nil {
			return nil, fmt.Errorf("secondary signing seed: %w", err)
		}
		s.secondary = sec
		s.keyID = keyID
	}
	return s, nil
}

// PublicKeyB64 returns the default key's public half, base64 (standard
// alphabet) — the value operators pin into the client build.
func (s *Signer) PublicKeyB64() string {
	return base64.StdEncoding.EncodeToString(pubOf(s.priv))
}

// SecondaryPublicKeyB64 returns the named key's public half, or "" when no
// rotation is configured.
func (s *Signer) SecondaryPublicKeyB64() string {
	if s.secondary == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(pubOf(s.secondary))
}

// pubOf derives the public half of an ed25519 private key. The conversion
// cannot fail for a well-formed key (seedFromHex validated the length).
func pubOf(k ed25519.PrivateKey) ed25519.PublicKey {
	pub, ok := k.Public().(ed25519.PublicKey)
	if !ok {
		return nil
	}
	return pub
}

// KeyID returns the named-key identifier, or "" when signing with the
// default key only.
func (s *Signer) KeyID() string { return s.keyID }

// Ready reports whether the signer has usable key material (health check).
func (s *Signer) Ready() bool { return len(s.priv) == ed25519.PrivateKeySize }

// Sign produces the detached signature over p's canonical bytes. The signing
// key follows p.SigningKeyID: nil signs with the default key; a non-nil id
// must match the configured named key (fail-closed — an unresolvable kid is
// never downgraded to the default key).
func (s *Signer) Sign(p *Payload) (payload, signatureB64 string, err error) {
	key := s.priv
	if p.SigningKeyID != nil {
		if s.secondary == nil || *p.SigningKeyID != s.keyID {
			return "", "", fmt.Errorf("signing key %q not configured", *p.SigningKeyID)
		}
		key = s.secondary
	}
	data := p.MarshalCanonical()
	sig := ed25519.Sign(key, data)
	return string(data), base64.StdEncoding.EncodeToString(sig), nil
}

// Verify checks a detached signature over payload bytes with pub. It is
// exercised by tests and golden vectors; the production client does the
// same check locally.
func Verify(pub ed25519.PublicKey, payload, signatureB64 string) bool {
	sig, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, []byte(payload), sig)
}
