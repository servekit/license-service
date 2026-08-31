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

// Payload is the cert v1 model (design doc §8.2, as amended: no default
// key). Invariants enforced by the signer's callers (admin grant +
// activation, both sides validated):
//
//   - perpetual entries have ExpiresAt == nil;
//   - subscription/trial entries always have ExpiresAt != nil;
//   - SigningKeyID is always set — every cert names the key that signed it,
//     and clients verify via their {kid → pubkey} table (fail-closed on
//     unknown kids).
type Payload struct {
	V             int
	CertID        string
	LicenseID     *string // nil = keyless trial cert
	DeviceToken   string
	FingerprintID string
	IssuedAt      time.Time
	SigningKeyID  string
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

// Signer holds the Ed25519 key material: a set of named keys (single-key
// deployments are just a set of one). There is no implicit default key —
// every payload names its key, every client verifies via its key table.
type Signer struct {
	keys   map[string]ed25519.PrivateKey // kid → key
	active string                        // kid that signs NEW payloads
}

// NewSigner builds a Signer from kid → 64-hex seed entries (at least one).
// signKeyID selects the key that signs NEW payloads via ActiveKeyID; when
// empty and exactly one key is configured, that key is active implicitly.
// A dangling signKeyID, an empty kid, or an ambiguous single-key default
// is a startup error, not a silent fallback.
func NewSigner(keysIn map[string]string, signKeyID string) (*Signer, error) {
	if len(keysIn) == 0 {
		return nil, fmt.Errorf("at least one signing key is required")
	}
	keys := make(map[string]ed25519.PrivateKey, len(keysIn))
	for kid, seedHex := range keysIn {
		if kid == "" {
			return nil, fmt.Errorf("signing key needs a non-empty key_id")
		}
		k, err := seedFromHex(seedHex)
		if err != nil {
			return nil, fmt.Errorf("signing key %q: %w", kid, err)
		}
		keys[kid] = k
	}
	active := signKeyID
	if active == "" {
		if len(keys) != 1 {
			return nil, fmt.Errorf("sign_key_id is required when multiple signing keys are configured")
		}
		for kid := range keys {
			active = kid
		}
	}
	if _, ok := keys[active]; !ok {
		return nil, fmt.Errorf("sign_key_id %q has no matching key", active)
	}
	return &Signer{keys: keys, active: active}, nil
}

// PublicKeys returns kid → base64 public key for every configured key — the
// values operators pin into client key tables.
func (s *Signer) PublicKeys() map[string]string {
	out := make(map[string]string, len(s.keys))
	for kid, k := range s.keys {
		out[kid] = base64.StdEncoding.EncodeToString(pubOf(k))
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

// ActiveKeyID is the kid stamped onto every NEW payload.
func (s *Signer) ActiveKeyID() string { return s.active }

// Ready reports whether the signer has usable key material (health check):
// an active key of the right size.
func (s *Signer) Ready() bool {
	k, ok := s.keys[s.active]
	return ok && len(k) == ed25519.PrivateKeySize
}

// Sign produces the detached signature over p's canonical bytes with the
// key named by p.SigningKeyID (fail-closed — an unconfigured kid is an
// error, never a fallback). ActiveKeyID deliberately does NOT apply here:
// Sign is literal about what the payload claims; choosing the active key
// for new payloads is the caller's (issuance path's) job.
func (s *Signer) Sign(p *Payload) (payload, signatureB64 string, err error) {
	key, ok := s.keys[p.SigningKeyID]
	if !ok {
		return "", "", fmt.Errorf("signing key %q not configured", p.SigningKeyID)
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
