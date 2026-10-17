package signin

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/mail"
	"github.com/vaxann/knowledge-base-mcp/internal/store"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)} }
func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func wrong(code string) string {
	if code == "000000" {
		return "000001"
	}
	return "000000"
}

func TestCodes(t *testing.T) {
	type step struct {
		advance time.Duration
		req     string
		code    func(issued string) string
		wantOK  bool
	}
	same := func(s string) string { return s }
	cases := []struct {
		name  string
		acct  string
		steps []step
	}{
		{"correct code", "acc1", []step{{0, "r1", same, true}}},
		{"just before expiry", "acc1", []step{{10 * time.Minute, "r1", same, true}}},
		{"expired", "acc1", []step{{11 * time.Minute, "r1", same, false}}},
		{"single use", "acc1", []step{{0, "r1", same, true}, {0, "r1", same, false}}},
		{"other sign-in request", "acc1", []step{{0, "r2", same, false}, {0, "r1", same, true}}},
		{"wrong then right", "acc1", []step{{0, "r1", wrong, false}, {0, "r1", same, true}}},
		{"four wrong then right", "acc1", []step{{0, "r1", wrong, false}, {0, "r1", wrong, false}, {0, "r1", wrong, false}, {0, "r1", wrong, false}, {0, "r1", same, true}}},
		{"five wrong exhausts", "acc1", []step{{0, "r1", wrong, false}, {0, "r1", wrong, false}, {0, "r1", wrong, false}, {0, "r1", wrong, false}, {0, "r1", wrong, false}, {0, "r1", same, false}}},
		{"decoy never succeeds", "", []step{{0, "r1", same, false}}},
		{"empty code", "acc1", []step{{0, "r1", func(string) string { return "" }, false}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ck := newClock()
			codes := NewCodes(ck.now)
			issued := codes.Issue("r1", c.acct)
			if !regexp.MustCompile(`^\d{6}$`).MatchString(issued) {
				t.Fatalf("code %q is not six digits", issued)
			}
			for i, s := range c.steps {
				ck.add(s.advance)
				acct, ok := codes.Verify(s.req, s.code(issued))
				if ok != s.wantOK {
					t.Fatalf("step %d: ok = %v, want %v", i, ok, s.wantOK)
				}
				if ok && acct != c.acct {
					t.Fatalf("step %d: account = %q", i, acct)
				}
			}
		})
	}
}

func TestCodesReissueReplaces(t *testing.T) {
	codes := NewCodes(newClock().now)
	first := codes.Issue("r1", "acc1")
	second := codes.Issue("r1", "acc1")
	for second == first { // one in a million
		second = codes.Issue("r1", "acc1")
	}
	if _, ok := codes.Verify("r1", first); ok {
		t.Fatal("the first code must stop working once a new one is issued")
	}
	if _, ok := codes.Verify("r1", second); !ok {
		t.Fatal("the new code must work")
	}
}

func TestCodesNeverIssued(t *testing.T) {
	codes := NewCodes(nil)
	if _, ok := codes.Verify("unknown", "123456"); ok {
		t.Fatal("a code that was never issued must fail")
	}
	codes.Issue("r1", "acc1")
	codes.Drop("r1")
	if len(codes.entries) != 0 {
		t.Error("Drop must forget the code")
	}
}

func TestCodesAreUniformEnough(t *testing.T) {
	seen := map[byte]int{}
	for i := 0; i < 2000; i++ {
		seen[newCode()[0]]++
	}
	if len(seen) != 10 {
		t.Errorf("leading digits seen: %v", seen)
	}
}

func TestLimiterPerAddress(t *testing.T) {
	ck := newClock()
	l := NewLimiter(DefaultLimits, ck.now)
	if !l.AllowRequest("a@example.com", "1.1.1.1") {
		t.Fatal("first request must pass")
	}
	if l.AllowRequest("a@example.com", "1.1.1.1") {
		t.Fatal("a second request within a minute must be refused")
	}
	if !l.AllowRequest("b@example.com", "1.1.1.1") {
		t.Fatal("another address is independent")
	}
	for i := 0; i < 4; i++ {
		ck.add(61 * time.Second)
		if !l.AllowRequest("a@example.com", "1.1.1.1") {
			t.Fatalf("request %d of the hour must pass", i+2)
		}
	}
	ck.add(61 * time.Second)
	if l.AllowRequest("a@example.com", "1.1.1.1") {
		t.Fatal("the sixth request within an hour must be refused")
	}
	ck.add(time.Hour)
	if !l.AllowRequest("a@example.com", "1.1.1.1") {
		t.Fatal("the window must slide")
	}
}

func TestLimiterPerClientAndTotal(t *testing.T) {
	ck := newClock()
	l := NewLimiter(Limits{AddrInterval: time.Minute, AddrPerHour: 5, ClientPerHour: 3, TotalPerHour: 2}, ck.now)
	for i, email := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		if !l.AllowRequest(email, "9.9.9.9") {
			t.Fatalf("request %d must pass", i)
		}
	}
	if l.AllowRequest("d@example.com", "9.9.9.9") {
		t.Fatal("the client limit must apply across addresses")
	}
	if !l.AllowRequest("d@example.com", "8.8.8.8") {
		t.Fatal("another client is independent")
	}
	first, second, third := l.AllowSend(), l.AllowSend(), l.AllowSend()
	if !first || !second || third {
		t.Fatal("the total limit must allow exactly two sends")
	}
	ck.add(61 * time.Minute)
	if !l.AllowSend() {
		t.Fatal("the total window must slide")
	}
}

func TestClientAddr(t *testing.T) {
	cases := []struct {
		name, remote, xff, want string
	}{
		{"direct public peer", "203.0.113.7:4000", "", "203.0.113.7"},
		{"public peer cannot spoof", "203.0.113.7:4000", "198.51.100.1", "203.0.113.7"},
		{"loopback proxy", "127.0.0.1:4000", "198.51.100.1", "198.51.100.1"},
		{"private proxy takes the last hop", "10.0.0.2:4000", "1.2.3.4, 198.51.100.9", "198.51.100.9"},
		{"loopback without header", "127.0.0.1:4000", "", "127.0.0.1"},
		{"garbage header", "127.0.0.1:4000", "not-an-ip", "127.0.0.1"},
		{"ipv6 loopback", "[::1]:4000", "2001:db8::1", "2001:db8::1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodGet, "http://kb.example.com/", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := ClientAddr(r); got != c.want {
				t.Errorf("ClientAddr = %q, want %q", got, c.want)
			}
		})
	}
}

// fakeSender records messages; fail makes delivery fail.
type fakeSender struct {
	mu   sync.Mutex
	msgs []mail.Message
	fail error
}

func (f *fakeSender) Send(_ context.Context, m mail.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.msgs = append(f.msgs, m)
	return nil
}

func (f *fakeSender) all() []mail.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mail.Message(nil), f.msgs...)
}

var codeRE = regexp.MustCompile(`\b\d{6}\b`)

func newFlow(t *testing.T) (*Flow, *fakeSender, *clock, *bytes.Buffer) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	ck := newClock()
	err = st.Update(func(s *store.State) error {
		if _, err := s.AddAccount("ann@example.com", false, ck.now()); err != nil {
			return err
		}
		off, err := s.AddAccount("off@example.com", false, ck.now())
		if err != nil {
			return err
		}
		off.Disabled = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	sender := &fakeSender{}
	f := &Flow{Store: st, Sender: sender, Codes: NewCodes(ck.now), Limiter: NewLimiter(DefaultLimits, ck.now),
		Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Now: ck.now}
	return f, sender, ck, logs
}

func TestFlowSuccess(t *testing.T) {
	f, sender, ck, logs := newFlow(t)
	f.Request("r1", " Ann@Example.com ", "1.1.1.1")
	f.Wait()
	msgs := sender.all()
	if len(msgs) != 1 || msgs[0].To != "ann@example.com" || msgs[0].IdempotencyKey == "" {
		t.Fatalf("messages = %+v", msgs)
	}
	code := codeRE.FindString(msgs[0].Text)
	ck.add(time.Minute)
	acc, ok := f.Verify("r1", " "+code+" ")
	if !ok || acc.Email != "ann@example.com" {
		t.Fatalf("Verify = %+v, %v", acc, ok)
	}
	if got := f.Store.Snapshot().Accounts[acc.ID].LastSignIn; !got.Equal(ck.now()) {
		t.Errorf("last sign-in = %v", got)
	}
	if _, ok := f.Verify("r1", code); ok {
		t.Error("the code must not work twice")
	}
	if strings.Contains(logs.String(), code) {
		t.Error("the code must never be logged")
	}
}

func TestFlowSendsNothingForUnknownOrDisabled(t *testing.T) {
	for _, email := range []string{"nobody@example.com", "off@example.com", "not-an-address", ""} {
		f, sender, _, _ := newFlow(t)
		before := len(f.Store.Snapshot().Accounts)
		f.Request("r1", email, "1.1.1.1")
		f.Wait()
		if n := len(sender.all()); n != 0 {
			t.Errorf("%q: %d emails sent, want none", email, n)
		}
		if len(f.Store.Snapshot().Accounts) != before {
			t.Errorf("%q: a sign-in attempt must not create an account", email)
		}
		for _, guess := range []string{"000000", "123456"} {
			if _, ok := f.Verify("r1", guess); ok {
				t.Errorf("%q: a decoy must never verify", email)
			}
		}
	}
}

func TestFlowRateLimit(t *testing.T) {
	f, sender, _, _ := newFlow(t)
	f.Request("r1", "ann@example.com", "1.1.1.1")
	f.Wait()
	first := codeRE.FindString(sender.all()[0].Text)
	f.Request("r1", "ann@example.com", "1.1.1.1") // within a minute
	f.Wait()
	if n := len(sender.all()); n != 1 {
		t.Fatalf("emails sent = %d, want 1", n)
	}
	if _, ok := f.Verify("r1", first); !ok {
		t.Error("a refused request must leave the earlier code valid")
	}
}

func TestFlowReissueInvalidatesEarlierCode(t *testing.T) {
	f, sender, ck, _ := newFlow(t)
	f.Request("r1", "ann@example.com", "1.1.1.1")
	f.Wait()
	ck.add(2 * time.Minute)
	f.Request("r1", "ann@example.com", "1.1.1.1")
	f.Wait()
	msgs := sender.all()
	if len(msgs) != 2 || msgs[0].IdempotencyKey == msgs[1].IdempotencyKey {
		t.Fatalf("messages = %+v", msgs)
	}
	first, second := codeRE.FindString(msgs[0].Text), codeRE.FindString(msgs[1].Text)
	if first != second {
		if _, ok := f.Verify("r1", first); ok {
			t.Error("the first code must stop working")
		}
	}
	if _, ok := f.Verify("r1", second); !ok {
		t.Error("the second code must work")
	}
}

func TestFlowDeliveryFailureIsInvisible(t *testing.T) {
	f, sender, _, logs := newFlow(t)
	sender.fail = errors.New("provider down")
	f.Request("r1", "ann@example.com", "1.1.1.1") // must not panic or block
	f.Wait()
	if !strings.Contains(logs.String(), "not delivered") {
		t.Error("a delivery failure must be logged")
	}
	if codeRE.MatchString(logs.String()) {
		t.Errorf("logs must not contain a code: %s", logs)
	}
}

func TestFlowAccountDisabledAfterCodeWasSent(t *testing.T) {
	f, sender, _, _ := newFlow(t)
	f.Request("r1", "ann@example.com", "1.1.1.1")
	f.Wait()
	code := codeRE.FindString(sender.all()[0].Text)
	if err := f.Store.Update(func(s *store.State) error { s.AccountByEmail("ann@example.com").Disabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Verify("r1", code); ok {
		t.Error("a code must not sign in an account disabled in the meantime")
	}
}
