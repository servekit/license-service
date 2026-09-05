package activation

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	licensev1 "github.com/servekit/api/gen/go/license/v1"
	"github.com/servekit/license-service/pkg/xcodes"
)

// normalizedKeyShape: "AV1D" + 20 chars of Crockford base32 (uppercase).
var normalizedKeyRe = regexp.MustCompile(`^AV1D[0-9A-Z]{20}$`)

// crockford is the display alphabet: base32 without I/L/O/U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NormalizeKey applies the dual-side normalization rule (design doc §8.3):
// trim → uppercase → strip non-alphanumerics → validate
// ^AV1D[0-9A-Z]{20}$. The plaintext key only exists transiently in memory;
// logs and the DB only ever see hashes or the 8-char prefix.
func NormalizeKey(raw string) (string, error) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	norm := b.String()
	if !normalizedKeyRe.MatchString(norm) {
		return "", xcodes.ErrBadKeyFormat.New()
	}
	return norm, nil
}

// KeyHash returns the 64-hex lowercase SHA-256 of the normalized key — the
// only key identity stored server-side.
func KeyHash(norm string) string {
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

// LicenseID derives the UI-facing key identity: "lk_" + first 32 hex chars.
// The prefix truncation collision probability is negligible and the unique
// index backstops it.
func LicenseID(keyHash string) string {
	return "lk_" + keyHash[:32]
}

// GenerateKey mints a fresh plaintext key "AV1D-XXXXX-XXXXX-XXXXX-XXXXX"
// (100 bits of entropy). Callers display it exactly once and store only the
// hash; uniqueness is guaranteed by entropy, with retry-on-collision at the
// caller.
func GenerateKey() (string, error) {
	groups := make([]byte, 4*5)
	if _, err := rand.Read(groups); err != nil {
		return "", fmt.Errorf("read entropy: %w", err)
	}
	var sb strings.Builder
	sb.WriteString("AV1D")
	for i := range groups {
		if i%5 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(crockford[groups[i]&31])
	}
	return sb.String(), nil
}

// moduleWireName maps the proto Module enum to the client contract wire name
// carried in cert entitlements. Unknown values return "" (never signed).
func moduleWireName(m licensev1.Module) string {
	switch m {
	case licensev1.Module_MODULE_DOWNLOADS:
		return "downloads"
	case licensev1.Module_MODULE_TOOLS:
		return "tools"
	default:
		return ""
	}
}

// moduleFromWire maps the client wire name back to the proto enum.
func moduleFromWire(name string) (licensev1.Module, error) {
	switch name {
	case "downloads":
		return licensev1.Module_MODULE_DOWNLOADS, nil
	case "tools":
		return licensev1.Module_MODULE_TOOLS, nil
	default:
		return licensev1.Module_MODULE_UNSPECIFIED,
			xcodes.ErrBadKeyFormat.New(fmt.Sprintf("unknown module %q", name))
	}
}

// moduleFromInt32 casts a stored int32 back to the proto enum (store
// boundary rule).
func moduleFromInt32(v int32) licensev1.Module { return licensev1.Module(v) }

// kindFromInt32 casts a stored int32 entitlement kind to the cert wire kind.
func kindFromInt32(v int32) (kind string, ok bool) {
	switch licensev1.EntitlementKind(v) {
	case licensev1.EntitlementKind_ENTITLEMENT_KIND_PERPETUAL:
		return "perpetual", true
	case licensev1.EntitlementKind_ENTITLEMENT_KIND_SUBSCRIPTION:
		return "subscription", true
	case licensev1.EntitlementKind_ENTITLEMENT_KIND_TRIAL:
		return "trial", true
	default:
		return "", false
	}
}
