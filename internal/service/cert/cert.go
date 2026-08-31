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

// defaultKeyID names the implicit default key (signingKeyId null in cert
// payloads — the key every shipped client pins as its fallback).
const defaultKeyID = ""

// Signer holds the Ed25519 key material: a mandatory default key plus any
// number of named keys (rotation transitions, per-build sharding).
type Signer struct {
	keys   map[string]ed25519.PrivateKey // kid → key; "" is the default
	active string                        // kid that signs NEW payloads; "" = default
}

// NewSigner builds a Signer from the default 64-hex seed plus optional named
// keys (kid → 64-hex seed). signKeyID selects which key signs NEW payloads
// via ActiveKeyID; "" (or absent) keeps the default. signKeyID must reference
// a configured named key — a dangling active key is a startup error, not a
// silent fallback.
func NewSigner(defaultSeedHex string, named map[string]string, signKeyID string) (*Signer, error) {
	def, err := seedFromHex(defaultSeedHex)
	if err != nil {
		return nil, fmt.Errorf("signing seed: %w", err)
	}
	keys := map[string]ed25519.PrivateKey{defaultKeyID: def}
	for kid, seedHex := range named {
		if kid == "" {
			return nil, fmt.Errorf("named signing key needs a non-empty key_id")
		}
		k, err := seedFromHex(seedHex)
		if err != nil {
			return nil, fmt.Errorf("named signing key %q: %w", kid, err)
		}
		keys[kid] = k
	}
	if signKeyID != "" {
		if _, ok := keys[signKeyID]; !ok {
			return nil, fmt.Errorf("sign_key_id %q has no matching named key", signKeyID)
		}
	}
	return &Signer{keys: keys, active: signKeyID}, nil
}

// PublicKeyB64 returns the default key's public half, base64 (standard
// alphabet) — the value operators pin into the client build.
func (s *Signer) PublicKeyB64() string {
	return base64.StdEncoding.EncodeToString(pubOf(s.keys[defaultKeyID]))
}

// NamedPublicKeys returns kid → base64 public key for every named key (the
// rotation/shard keys clients can pin into their key table).
func (s *Signer) NamedPublicKeys() map[string]string {
	out := make(map[string]string, len(s.keys)-1)
	for kid, k := range s.keys {
		if kid != defaultKeyID {
			out[kid] = base64.StdEncoding.EncodeToString(pubOf(k))
		}
	}
	return out
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

// ActiveKeyID is the kid stamped onto NEW payloads by the issuance path
// ("" = sign with the default key, signingKeyId omitted from the payload).
func (s *Signer) ActiveKeyID() string { return s.active }

// Ready reports whether the signer has usable key material (health check).
func (s *Signer) Ready() bool {
	k, ok := s.keys[defaultKeyID]
	return ok && len(k) == ed25519.PrivateKeySize
}

// Sign produces the detached signature over p's canonical bytes. The signing
// key follows p.SigningKeyID: nil signs with the default key; a non-nil id
// must match a configured named key (fail-closed — an unresolvable kid is
// never downgraded to the default key). ActiveKeyID deliberately does NOT
// apply here: Sign is literal about what the payload claims; choosing the
// active key for new payloads is the caller's (issuance path's) job.
func (s *Signer) Sign(p *Payload) (payload, signatureB64 string, err error) {
	kid := defaultKeyID
	if p.SigningKeyID != nil {
		kid = *p.SigningKeyID
	}
	key, ok := s.keys[kid]
	if !ok {
		return "", "", fmt.Errorf("signing key %q not configured", kid)
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
