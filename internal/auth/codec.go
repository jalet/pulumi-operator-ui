// Package auth implements OIDC login, signed sessions and the claim allowlist.
package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	keyBytesMin = 32
	// Browsers cap a cookie at about 4 KiB; anything larger was not issued by us.
	cookieBytesMax = 4096
)

// Codec errors.
var (
	ErrInvalidToken = errors.New("invalid token")
	ErrKeyTooShort  = errors.New("session key must be at least 32 bytes")
)

// Session is the signed content of the session cookie. It holds identity only, never
// tokens.
type Session struct {
	Subject   string    `json:"sub"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	IssuedAt  time.Time `json:"iat"`
	ExpiresAt time.Time `json:"exp"`
}

// Codec signs and verifies tokens with HMAC-SHA256. The first key signs; every key
// verifies, so a rotated-out key keeps existing sessions valid until they expire.
type Codec struct {
	keys [][]byte
}

// LoadKey reads a key file, trimming surrounding whitespace.
func LoadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is operator configuration
	if err != nil {
		return nil, fmt.Errorf("read session key: %w", err)
	}
	key := bytes.TrimSpace(b)
	if len(key) < keyBytesMin {
		return nil, fmt.Errorf("read session key %s: %w", path, ErrKeyTooShort)
	}
	return key, nil
}

// NewCodec builds a codec that signs with current and also accepts previous (may be nil).
func NewCodec(current, previous []byte) (*Codec, error) {
	if len(current) < keyBytesMin {
		return nil, ErrKeyTooShort
	}
	keys := [][]byte{bytes.Clone(current)}
	if previous != nil {
		if len(previous) < keyBytesMin {
			return nil, fmt.Errorf("previous key: %w", ErrKeyTooShort)
		}
		keys = append(keys, bytes.Clone(previous))
	}
	return &Codec{keys: keys}, nil
}

// Seal signs payload for purpose kind ("session" or "flow"). A token sealed for one kind
// never opens as another.
func (c *Codec) Seal(kind string, payload []byte) string {
	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + base64.RawURLEncoding.EncodeToString(mac(c.keys[0], kind, body))
}

// Open verifies token for kind and returns its payload.
func (c *Codec) Open(kind, token string) ([]byte, error) {
	if len(token) > cookieBytesMax {
		return nil, ErrInvalidToken
	}
	body, sigText, ok := strings.Cut(token, ".")
	if !ok {
		return nil, ErrInvalidToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigText)
	if err != nil {
		return nil, ErrInvalidToken
	}
	for _, k := range c.keys {
		if hmac.Equal(sig, mac(k, kind, body)) {
			payload, err := base64.RawURLEncoding.DecodeString(body)
			if err != nil {
				return nil, ErrInvalidToken
			}
			return payload, nil
		}
	}
	return nil, ErrInvalidToken
}

func mac(key []byte, kind, body string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(kind + "." + body)) // hash.Hash.Write never returns an error
	return h.Sum(nil)
}
