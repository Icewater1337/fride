// Package companion is FrIDE's side of companion mode: a program on the same
// computer (a node agent of some larger system, a desktop assistant) that runs
// FrIDE, calls its API on the user's behalf and relays thread news.
//
// The companion is paired once. `fride pair-companion` prints a one-time code;
// the companion trades it for a long-lived bearer token at
// POST /api/companion/pair, from loopback only. FrIDE keeps only the token's
// SHA-256, so a copy of its state folder does not let anyone call the API as
// the companion. The same hash is the key thread news is signed with, which
// the companion, holding the token, can derive and nobody else can.
//
// There is one companion at a time: pairing again replaces the token.
package companion

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/settings"
)

const (
	// CodeTTL is how long a pairing code can be redeemed. Long enough to copy
	// it into the companion, short enough that a code printed into a
	// terminal's scrollback is worthless soon after.
	CodeTTL = 10 * time.Minute
	// maxTries burns a code after this many wrong guesses.
	maxTries = 5
	// codeAlphabet leaves out 0/O and 1/I/L, which are misread when a code is
	// typed from a screen.
	codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	tokenPrefix  = "frc_"
)

var (
	// ErrNoCode: nothing to redeem — no code was made, it was used, or it
	// expired.
	ErrNoCode = errors.New("no pairing code is pending — run `fride pair-companion` and use the code it prints")
	// ErrBadCode: a code is pending and this is not it.
	ErrBadCode = errors.New("wrong pairing code")
)

// mu serialises redeeming within the server. Across processes the code file
// is consumed with os.Remove, which succeeds for one caller only.
var mu sync.Mutex

// Dir is where the pairing lives: <state>/companion, 0700.
func Dir(stateDir string) string { return filepath.Join(stateDir, "companion") }

func codePath(stateDir string) string  { return filepath.Join(Dir(stateDir), "pairing-code.json") }
func tokenPath(stateDir string) string { return filepath.Join(Dir(stateDir), "token.json") }

type pending struct {
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
	Tries   int       `json:"tries,omitempty"`
}

// Pairing is the stored half of a pairing: never the token itself.
type Pairing struct {
	// Hash is the hex SHA-256 of the token.
	Hash   string    `json:"hash"`
	Paired time.Time `json:"paired"`
	// Webhook is the URL the companion asked thread news to go to, if any.
	Webhook string `json:"webhookUrl,omitempty"`
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// normalizeCode accepts a code however it was retyped: any case, with or
// without the dashes, with stray spaces.
func normalizeCode(c string) string {
	c = strings.ToUpper(c)
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, c)
}

func writePrivate(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// NewCode makes a pairing code, replacing one that is pending. 16 characters
// from a 31-letter alphabet: about 79 bits, printed as XXXX-XXXX-XXXX-XXXX.
func NewCode(stateDir string, now time.Time) (code string, expires time.Time, err error) {
	var b strings.Builder
	buf := make([]byte, 1)
	// Rejection sampling: a byte modulo 31 would favour the first letters.
	limit := byte(256 / len(codeAlphabet) * len(codeAlphabet))
	for n := 0; n < 16; {
		if _, err := rand.Read(buf); err != nil {
			return "", time.Time{}, err
		}
		if buf[0] >= limit {
			continue
		}
		if n > 0 && n%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(codeAlphabet[int(buf[0])%len(codeAlphabet)])
		n++
	}
	code = b.String()
	expires = now.Add(CodeTTL)
	if err := writePrivate(codePath(stateDir), pending{Hash: hashHex(normalizeCode(code)), Expires: expires}); err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

// Redeem trades a pending code for a new token, once. webhook is the URL the
// companion wants thread news at; "" for none, otherwise loopback only.
func Redeem(stateDir, code, webhook string, now time.Time) (string, error) {
	if webhook = strings.TrimSpace(webhook); webhook != "" {
		if err := settings.LoopbackURL(webhook); err != nil {
			return "", fmt.Errorf("webhookUrl %w", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	path := codePath(stateDir)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoCode
	}
	if err != nil {
		return "", err
	}
	var p pending
	if err := json.Unmarshal(b, &p); err != nil || p.Hash == "" {
		_ = os.Remove(path)
		return "", ErrNoCode
	}
	if !now.Before(p.Expires) {
		_ = os.Remove(path)
		return "", ErrNoCode
	}
	got := hashHex(normalizeCode(code))
	if subtle.ConstantTimeCompare([]byte(got), []byte(p.Hash)) != 1 {
		p.Tries++
		if p.Tries >= maxTries {
			_ = os.Remove(path)
		} else {
			_ = writePrivate(path, p)
		}
		return "", ErrBadCode
	}
	// Consumed before the token exists: whatever happens next, this code
	// cannot be used a second time.
	if err := os.Remove(path); err != nil {
		return "", ErrNoCode
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	if err := writePrivate(tokenPath(stateDir), Pairing{Hash: hashHex(token), Paired: now, Webhook: webhook}); err != nil {
		return "", err
	}
	return token, nil
}

// Current is the stored pairing, nil when no companion is paired.
func Current(stateDir string) *Pairing {
	b, err := os.ReadFile(tokenPath(stateDir))
	if err != nil {
		return nil
	}
	var p Pairing
	if json.Unmarshal(b, &p) != nil || len(p.Hash) != sha256.Size*2 {
		return nil
	}
	return &p
}

// Verify reports whether token is the paired companion's.
func Verify(stateDir, token string) bool {
	p := Current(stateDir)
	if p == nil || !strings.HasPrefix(token, tokenPrefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashHex(token)), []byte(p.Hash)) == 1
}

// Key is the webhook signing key of a pairing: SHA-256(token), as raw bytes.
func (p *Pairing) Key() []byte {
	if p == nil {
		return nil
	}
	k, err := hex.DecodeString(p.Hash)
	if err != nil {
		return nil
	}
	return k
}

// KeyForToken is the signing key as the companion derives it from its token.
func KeyForToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Sign is the X-Fride-Signature value for body: "sha256=" and the hex
// HMAC-SHA256 of the exact body bytes under key.
func Sign(key, body []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Bearer pulls the token out of an Authorization header.
func Bearer(h string) string {
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}
