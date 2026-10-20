package admin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// A cursor is opaque and signed (api-conventions.md section 9, D6): the
// position of the last row of a page and a digest of the filters that produced
// it, base64url, then a dot and an HMAC-SHA256 over the first part under the
// key every instance shares. A tampered cursor, one replayed under different
// filters, and one signed by a key the store has since lost are all a 400 and
// a re-read, never a silent full scan or a wrong page.

type cursorBody struct {
	At     int64  `json:"t"` // unix nanoseconds
	Tie    string `json:"i"`
	Filter string `json:"f"`
}

var errBadCursor = errors.New("admin: not a cursor this service issued")

// signer caches the shared key for a short while. A key the store lost and
// another instance re-created is picked up within keyTTL, so the instances
// converge on one key again rather than rejecting each other's cursors for
// the life of the process.
type signer struct {
	mu   sync.Mutex
	key  []byte
	read time.Time
}

const keyTTL = 30 * time.Second

func (s *Service) cursorKey(ctx context.Context) ([]byte, error) {
	s.sign.mu.Lock()
	defer s.sign.mu.Unlock()
	if s.sign.key != nil && time.Since(s.sign.read) < keyTTL {
		return s.sign.key, nil
	}
	k, err := s.data.CursorKey(ctx)
	if err != nil {
		return nil, err
	}
	s.sign.key, s.sign.read = k, time.Now()
	return k, nil
}

// filterDigest binds a cursor to the route and the filters it was issued
// under.
func filterDigest(route string, filters ...string) string {
	h := sha256.Sum256([]byte(route + "\x00" + strings.Join(filters, "\x00")))
	return base64.RawURLEncoding.EncodeToString(h[:9])
}

func mac(key, payload []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(payload)
	return m.Sum(nil)
}

func (s *Service) encodeCursor(ctx context.Context, p Position, filter string) (string, error) {
	key, err := s.cursorKey(ctx)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(cursorBody{At: p.At.UnixNano(), Tie: p.Tie, Filter: filter})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac(key, payload)), nil
}

// decodeCursor returns errBadCursor for anything this service did not issue
// under these filters, and the store's error when the key cannot be read.
func (s *Service) decodeCursor(ctx context.Context, raw, filter string) (*Position, error) {
	if raw == "" {
		return nil, nil
	}
	left, right, ok := strings.Cut(raw, ".")
	if !ok {
		return nil, errBadCursor
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(left)
	sig, err2 := base64.RawURLEncoding.DecodeString(right)
	if err1 != nil || err2 != nil {
		return nil, errBadCursor
	}
	key, err := s.cursorKey(ctx)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal(sig, mac(key, payload)) {
		return nil, errBadCursor
	}
	var b cursorBody
	if err := json.Unmarshal(payload, &b); err != nil || b.Filter != filter {
		return nil, errBadCursor
	}
	return &Position{At: time.Unix(0, b.At).UTC(), Tie: b.Tie}, nil
}
