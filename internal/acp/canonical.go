// Package acp implements the Agentic Commerce Protocol (ACP, spec
// 2026-04-17, github.com/agentic-commerce-protocol) for UPI: a merchant
// checkout API, a PSP delegate-payment API whose payment method is a UPI
// Reserve Pay block, signed user mandates, and the protocol's idempotency
// rules. ACP's delegate-payment spec defines only cards; the UPI payment
// method and the mandate are this project's extension, kept in the
// protocol's shapes (allowance, vault token, payment handler, flat errors).
package acp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
)

// Canonical returns a canonical JSON encoding (sorted keys, no spaces,
// numbers normalised so 1.0 and 1 are equal), in the spirit of RFC 8785.
// ACP 6.2: key order does not matter, null differs from absent, arrays are
// order-sensitive.
func Canonical(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("acp: trailing data after JSON value")
	}
	var b bytes.Buffer
	if err := write(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Fingerprint is the SHA-256 of the canonical form.
func Fingerprint(raw []byte) (string, error) {
	c, err := Canonical(raw)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(c)
	return hex.EncodeToString(s[:]), nil
}

// CanonicalValue encodes a Go value canonically (for signing).
func CanonicalValue(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return Canonical(raw)
}

func write(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		enc, _ := json.Marshal(x)
		b.Write(enc)
	case json.Number:
		f, ok := new(big.Float).SetPrec(200).SetString(string(x))
		if !ok {
			return fmt.Errorf("acp: bad number %q", x)
		}
		if f.IsInt() {
			i, _ := f.Int(nil)
			b.WriteString(i.String())
		} else {
			b.WriteString(f.Text('g', -1))
		}
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := write(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			enc, _ := json.Marshal(k)
			b.Write(enc)
			b.WriteByte(':')
			if err := write(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("acp: unexpected JSON type %T", v)
	}
	return nil
}
