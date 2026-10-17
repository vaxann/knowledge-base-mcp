package signin

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/mail"
	"github.com/vaxann/knowledge-base-mcp/internal/store"
)

// Flow is the email sign-in used by both the OAuth sign-in page and the admin
// panel: request a code for an address, then exchange the code for an account.
type Flow struct {
	Store   *store.Store
	Sender  mail.Sender
	Codes   *Codes
	Limiter *Limiter
	Log     *slog.Logger
	// Now is the clock (wall clock when nil).
	Now func() time.Time

	wg   sync.WaitGroup
	seq  sync.Mutex
	sent map[string]int // sign-in request id -> emails sent, for idempotency keys
}

// NewFlow wires a flow with default limits.
func NewFlow(st *store.Store, sender mail.Sender, log *slog.Logger) *Flow {
	if log == nil {
		log = slog.Default()
	}
	return &Flow{Store: st, Sender: sender, Codes: NewCodes(nil), Limiter: NewLimiter(DefaultLimits, nil), Log: log}
}

func (f *Flow) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Request asks for a code to be sent to an address for a sign-in request.
// It returns nothing on purpose: the caller must respond identically whether
// the address is known, disabled, unknown, malformed or over a limit, and
// whether or not delivery works.
func (f *Flow) Request(reqID, email, client string) {
	norm, err := store.NormalizeEmail(email)
	if err != nil {
		norm = strings.ToLower(strings.TrimSpace(email))
	}
	if !f.Limiter.AllowRequest(norm, client) {
		f.Log.Warn("sign-in: code request over the limit", "email", norm)
		return
	}
	var accountID string
	if acc := f.Store.Snapshot().AccountByEmail(norm); err == nil && acc != nil && !acc.Disabled {
		accountID = acc.ID
	}
	code := f.Codes.Issue(reqID, accountID)
	if accountID == "" {
		f.Log.Info("sign-in: code requested for an address without an active account", "email", norm)
		return
	}
	if !f.Limiter.AllowSend() {
		f.Log.Warn("sign-in: hourly email limit reached, not sending", "email", norm)
		return
	}
	f.seq.Lock()
	if f.sent == nil {
		f.sent = map[string]int{}
	}
	f.sent[reqID]++
	key := fmt.Sprintf("%s/%d", reqID, f.sent[reqID])
	f.seq.Unlock()
	msg := mail.CodeMessage(norm, code, f.Codes.TTL(), key)
	// Delivery runs in the background so the response time does not depend
	// on the mail provider and cannot tell a known address from an unknown one.
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := f.Sender.Send(ctx, msg); err != nil {
			f.Log.Error("sign-in: code email was not delivered", "email", norm, "err", err)
			return
		}
		f.Log.Info("sign-in: code sent", "email", norm)
	}()
}

// Verify exchanges a code for the account it was issued to. It fails for a
// wrong, expired, exhausted or foreign code and for an account that was
// disabled or deleted in the meantime; the caller cannot tell which.
func (f *Flow) Verify(reqID, code string) (*store.Account, bool) {
	id, ok := f.Codes.Verify(reqID, strings.TrimSpace(code))
	if !ok {
		return nil, false
	}
	acc := f.Store.Snapshot().Accounts[id]
	if acc == nil || acc.Disabled {
		return nil, false
	}
	f.seq.Lock()
	delete(f.sent, reqID)
	f.seq.Unlock()
	now := f.now()
	if err := f.Store.Update(func(st *store.State) error {
		if a := st.Accounts[id]; a != nil {
			a.LastSignIn = now
		}
		return nil
	}); err != nil {
		f.Log.Error("sign-in: cannot record the sign-in time", "err", err)
	}
	f.Log.Info("sign-in: succeeded", "email", acc.Email)
	return acc, true
}

// Wait blocks until background deliveries finish (tests and shutdown).
func (f *Flow) Wait() { f.wg.Wait() }
