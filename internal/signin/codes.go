// Package signin implements sign-in with a one-time code sent by email: code
// issue and verification, request rate limits and the flow that ties them to
// accounts and mail delivery.
package signin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// Defaults of the code policy.
const (
	CodeTTL     = 10 * time.Minute
	MaxAttempts = 5
)

type entry struct {
	accountID string // "" for a decoy issued to an unknown or disabled address
	salt      []byte
	hash      []byte
	expires   time.Time
	attempts  int
}

// Codes issues and verifies one-time codes. A code lives in memory only, as
// a salted hash, bound to the sign-in request that asked for it.
type Codes struct {
	mu      sync.Mutex
	entries map[string]*entry // sign-in request id -> pending code
	now     func() time.Time
	ttl     time.Duration
	max     int
}

// NewCodes builds a manager; now may be nil (wall clock).
func NewCodes(now func() time.Time) *Codes {
	if now == nil {
		now = time.Now
	}
	return &Codes{entries: map[string]*entry{}, now: now, ttl: CodeTTL, max: MaxAttempts}
}

// TTL returns the lifetime of a code.
func (c *Codes) TTL() time.Duration { return c.ttl }

func newCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%06d", n.Int64())
}

func hashCode(salt []byte, code string) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(code))
	return h.Sum(nil)
}

// Issue creates a code for a sign-in request, replacing any earlier one. An
// empty accountID issues a decoy: it is tracked like a real code so that
// verification behaves the same, but it can never succeed.
func (c *Codes) Issue(reqID, accountID string) string {
	code := newCode()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, e := range c.entries {
		if now.After(e.expires) {
			delete(c.entries, k)
		}
	}
	c.entries[reqID] = &entry{accountID: accountID, salt: salt, hash: hashCode(salt, code), expires: now.Add(c.ttl)}
	return code
}

// Verify checks a code for a sign-in request. On success it returns the
// account id and consumes the code. Every failure looks the same to the
// caller: wrong, expired, exhausted, a decoy, or never issued.
func (c *Codes) Verify(reqID, code string) (accountID string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[reqID]
	if e == nil {
		// Burn the same work as a real comparison.
		hmac.Equal(hashCode(make([]byte, 16), code), make([]byte, sha256.Size))
		return "", false
	}
	if c.now().After(e.expires) {
		delete(c.entries, reqID)
		return "", false
	}
	if e.attempts >= c.max {
		return "", false
	}
	match := hmac.Equal(hashCode(e.salt, code), e.hash)
	if !match || e.accountID == "" {
		e.attempts++
		return "", false
	}
	delete(c.entries, reqID)
	return e.accountID, true
}

// Drop forgets the pending code of a sign-in request.
func (c *Codes) Drop(reqID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, reqID)
}
