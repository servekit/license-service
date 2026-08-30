package cert

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
)

// seedFromHex decodes a 64-hex (32-byte) Ed25519 seed into a full private
// key. Seeds are injected via ${LICENSE_SIGNING_SEED}; they are never stored
// in the DB, committed, or logged.
func seedFromHex(seedHex string) (ed25519.PrivateKey, error) {
	if seedHex == "" {
		return nil, fmt.Errorf("empty seed")
	}
	seed, err := hex.DecodeString(seedHex)
	if err != nil {
		return nil, fmt.Errorf("decode hex: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
