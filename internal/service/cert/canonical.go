// Package cert implements the license credential pipeline: a hand-written
// deterministic canonical JSON serializer, the cert v1 payload model, and
// Ed25519 detached signing.
//
// The client re-computes the canonical form of the payload it received and
// asserts byte-for-byte equality with the wire payload (facade.rs), so
// MarshalCanonical must be exactly the RFC 8785 subset described in
// docs/design.md §8.1 — no reflection, no map-iteration order, no float
// formatting surprises.
package cert

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// buf is a tiny append-only JSON writer with canonical escaping.
type buf struct{ b []byte }

// str appends a JSON string with canonical escaping: only `"`, `\`, the
// short escapes (\b \f \n \r \t) and U+0000–U+001F are escaped (\u00xx,
// lowercase hex); every other byte passes through verbatim.
func (w *buf) str(s string) {
	w.b = append(w.b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			w.b = append(w.b, '\\', '"')
		case '\\':
			w.b = append(w.b, '\\', '\\')
		case '\b':
			w.b = append(w.b, '\\', 'b')
		case '\f':
			w.b = append(w.b, '\\', 'f')
		case '\n':
			w.b = append(w.b, '\\', 'n')
		case '\r':
			w.b = append(w.b, '\\', 'r')
		case '\t':
			w.b = append(w.b, '\\', 't')
		default:
			if c < 0x20 {
				const hex = "0123456789abcdef"
				w.b = append(w.b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			} else {
				w.b = append(w.b, c)
			}
		}
	}
	w.b = append(w.b, '"')
}

// raw appends pre-serialized fragments (numbers, null, already-encoded parts).
func (w *buf) raw(s string) { w.b = append(w.b, s...) }

// canonicalEntitlement serializes one entitlement entry. Both members are
// always present; expiresAt is null for perpetual.
func canonicalEntitlement(w *buf, e Entitlement) {
	w.raw("{")
	w.str("expiresAt")
	w.raw(":")
	if e.ExpiresAt == nil {
		w.raw("null")
	} else {
		w.str(formatTime(*e.ExpiresAt))
	}
	w.raw(",")
	w.str("kind")
	w.raw(":")
	w.str(string(e.Kind))
	w.raw("}")
}

// MarshalCanonical serializes p to its canonical JSON bytes.
//
// Layout rules (byte order, ascending):
//
//	certId, deviceToken, entitlements, fingerprintId, issuedAt,
//	licenseId, [signingKeyId], v
//
// licenseId is always present (null for keyless trial certs);
// signingKeyId is omitted when nil (the golden vectors pin this: the
// default-key vector carries no signingKeyId member) and must sort between
// licenseId and v when set. Entitlement keys (module names) are sorted
// bytewise. issuedAt is RFC3339 UTC at second precision.
func (p *Payload) MarshalCanonical() []byte {
	w := &buf{}

	w.raw("{")
	w.str("certId")
	w.raw(":")
	w.str(p.CertID)
	w.raw(",")
	w.str("deviceToken")
	w.raw(":")
	w.str(p.DeviceToken)
	w.raw(",")
	w.str("entitlements")
	w.raw(":{")
	modules := make([]string, 0, len(p.Entitlements))
	for m := range p.Entitlements {
		modules = append(modules, m)
	}
	sort.Strings(modules)
	for i, m := range modules {
		if i > 0 {
			w.raw(",")
		}
		w.str(m)
		w.raw(":")
		canonicalEntitlement(w, p.Entitlements[m])
	}
	w.raw("}")
	w.raw(",")
	w.str("fingerprintId")
	w.raw(":")
	w.str(p.FingerprintID)
	w.raw(",")
	w.str("issuedAt")
	w.raw(":")
	w.str(formatTime(p.IssuedAt))
	w.raw(",")
	w.str("licenseId")
	w.raw(":")
	if p.LicenseID == nil {
		w.raw("null")
	} else {
		w.str(*p.LicenseID)
	}
	w.raw(",")
	if p.SigningKeyID != nil {
		w.str("signingKeyId")
		w.raw(":")
		w.str(*p.SigningKeyID)
		w.raw(",")
	}
	w.str("v")
	w.raw(":")
	w.raw(fmt.Sprintf("%d", p.V))
	w.raw("}")

	return w.b
}

// UnmarshalCanonical parses canonical JSON back into a Payload. It exists so
// tests can prove marshal→unmarshal→marshal stability, and so future tooling
// can inspect issued certs without re-deriving them.
func UnmarshalCanonical(data []byte) (*Payload, error) {
	val, rest, err := parseValue(data)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(rest)) != "" {
		return nil, fmt.Errorf("trailing bytes after canonical JSON")
	}
	obj, ok := val.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("cert payload must be a JSON object")
	}
	p := &Payload{}
	for k, v := range obj {
		switch k {
		case "v":
			f, ok := v.(float64)
			if !ok {
				return nil, fmt.Errorf("v must be a number")
			}
			p.V = int(f)
		case "certId":
			p.CertID, ok = v.(string)
			if !ok {
				return nil, fmt.Errorf("certId must be a string")
			}
		case "deviceToken":
			p.DeviceToken, ok = v.(string)
			if !ok {
				return nil, fmt.Errorf("deviceToken must be a string")
			}
		case "fingerprintId":
			p.FingerprintID, ok = v.(string)
			if !ok {
				return nil, fmt.Errorf("fingerprintId must be a string")
			}
		case "issuedAt":
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("issuedAt must be a string")
			}
			p.IssuedAt, err = parseTime(s)
			if err != nil {
				return nil, err
			}
		case "licenseId":
			if v == nil {
				p.LicenseID = nil
			} else {
				s, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf("licenseId must be a string or null")
				}
				p.LicenseID = &s
			}
		case "signingKeyId":
			if v == nil {
				p.SigningKeyID = nil
			} else {
				s, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf("signingKeyId must be a string or null")
				}
				p.SigningKeyID = &s
			}
		case "entitlements":
			ents, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("entitlements must be an object")
			}
			p.Entitlements = map[string]Entitlement{}
			for m, ev := range ents {
				eo, ok := ev.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("entitlement %q must be an object", m)
				}
				var e Entitlement
				kind, ok := eo["kind"].(string)
				if !ok {
					return nil, fmt.Errorf("entitlement %q missing kind", m)
				}
				e.Kind = Kind(kind)
				if exp, present := eo["expiresAt"]; present && exp != nil {
					s, ok := exp.(string)
					if !ok {
						return nil, fmt.Errorf("entitlement %q expiresAt must be a string", m)
					}
					e.ExpiresAt = new(time.Time)
					*e.ExpiresAt, err = parseTime(s)
					if err != nil {
						return nil, err
					}
				}
				p.Entitlements[m] = e
			}
		default:
			// Unknown top-level members are ignored (client rule).
		}
	}
	return p, nil
}

// parseValue parses one JSON value at the head of data, returning the value
// and the remainder. Objects become map[string]any regardless of member
// order; this parser only feeds UnmarshalCanonical for round-trip tests.
func parseValue(data []byte) (any, []byte, error) {
	data = skipWS(data)
	if len(data) == 0 {
		return nil, nil, fmt.Errorf("unexpected end of JSON")
	}
	switch data[0] {
	case '{':
		obj := map[string]any{}
		rest := skipWS(data[1:])
		if len(rest) > 0 && rest[0] == '}' {
			return obj, rest[1:], nil
		}
		for {
			var key string
			var err error
			key, rest, err = parseString(rest)
			if err != nil {
				return nil, nil, err
			}
			rest = skipWS(rest)
			if len(rest) == 0 || rest[0] != ':' {
				return nil, nil, fmt.Errorf("expected ':' in object")
			}
			var val any
			val, rest, err = parseValue(rest[1:])
			if err != nil {
				return nil, nil, err
			}
			obj[key] = val
			rest = skipWS(rest)
			if len(rest) == 0 {
				return nil, nil, fmt.Errorf("unterminated object")
			}
			if rest[0] == ',' {
				rest = skipWS(rest[1:])
				continue
			}
			if rest[0] == '}' {
				return obj, rest[1:], nil
			}
			return nil, nil, fmt.Errorf("expected ',' or '}' in object")
		}
	case '"':
		s, rest, err := parseString(data)
		return s, rest, err
	case 'n':
		if strings.HasPrefix(string(data), "null") {
			return nil, data[4:], nil
		}
		return nil, nil, fmt.Errorf("bad literal")
	default:
		// number (small integers only in our value domain)
		i := 0
		for i < len(data) && (data[i] == '-' || data[i] == '+' || data[i] == '.' ||
			data[i] == 'e' || data[i] == 'E' || (data[i] >= '0' && data[i] <= '9')) {
			i++
		}
		var f float64
		if _, err := fmt.Sscanf(string(data[:i]), "%g", &f); err != nil {
			return nil, nil, fmt.Errorf("bad number")
		}
		return f, data[i:], nil
	}
}

func skipWS(data []byte) []byte {
	for len(data) > 0 {
		switch data[0] {
		case ' ', '\t', '\n', '\r':
			data = data[1:]
		default:
			return data
		}
	}
	return data
}

// parseString parses a JSON string (with unescaping) at the head of data.
func parseString(data []byte) (string, []byte, error) {
	data = skipWS(data)
	if len(data) == 0 || data[0] != '"' {
		return "", nil, fmt.Errorf("expected string")
	}
	var sb strings.Builder
	for i := 1; i < len(data); i++ {
		c := data[i]
		switch c {
		case '"':
			return sb.String(), data[i+1:], nil
		case '\\':
			if i+1 >= len(data) {
				return "", nil, fmt.Errorf("unterminated escape")
			}
			switch data[i+1] {
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'u':
				if i+5 >= len(data) {
					return "", nil, fmt.Errorf("bad \\u escape")
				}
				var r rune
				if _, err := fmt.Sscanf(string(data[i+2:i+6]), "%04x", &r); err != nil {
					return "", nil, fmt.Errorf("bad \\u escape")
				}
				sb.WriteRune(r)
				i += 4
			default:
				return "", nil, fmt.Errorf("bad escape")
			}
			i++
		default:
			sb.WriteByte(c)
		}
	}
	return "", nil, fmt.Errorf("unterminated string")
}
