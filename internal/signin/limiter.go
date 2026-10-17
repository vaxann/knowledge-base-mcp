package signin

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Limits bound how often codes are requested and sent.
type Limits struct {
	AddrInterval  time.Duration // minimum gap between codes for one address
	AddrPerHour   int
	ClientPerHour int // per client network address
	TotalPerHour  int // emails actually sent, all addresses together
}

// DefaultLimits are the limits used unless a test overrides them.
var DefaultLimits = Limits{AddrInterval: time.Minute, AddrPerHour: 5, ClientPerHour: 20, TotalPerHour: 100}

// Limiter enforces Limits with sliding one-hour windows.
type Limiter struct {
	mu     sync.Mutex
	now    func() time.Time
	lim    Limits
	addr   map[string][]time.Time
	client map[string][]time.Time
	total  []time.Time
}

// NewLimiter builds a limiter; now may be nil (wall clock).
func NewLimiter(lim Limits, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, lim: lim, addr: map[string][]time.Time{}, client: map[string][]time.Time{}}
}

func prune(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	return ts[i:]
}

// AllowRequest reports whether a code may be issued for this address from
// this client, and records the request when it may. It counts every request,
// whether or not the address has an account.
func (l *Limiter) AllowRequest(email, client string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-time.Hour)
	for k, ts := range l.addr {
		if ts = prune(ts, cutoff); len(ts) == 0 {
			delete(l.addr, k)
		} else {
			l.addr[k] = ts
		}
	}
	for k, ts := range l.client {
		if ts = prune(ts, cutoff); len(ts) == 0 {
			delete(l.client, k)
		} else {
			l.client[k] = ts
		}
	}
	a, c := l.addr[email], l.client[client]
	if len(a) > 0 && now.Sub(a[len(a)-1]) < l.lim.AddrInterval {
		return false
	}
	if len(a) >= l.lim.AddrPerHour || len(c) >= l.lim.ClientPerHour {
		return false
	}
	l.addr[email] = append(a, now)
	l.client[client] = append(c, now)
	return true
}

// AllowSend reports whether one more email fits the hourly total, and
// records it when it does. Only real deliveries call it, so requests for
// unknown addresses cannot use up the budget.
func (l *Limiter) AllowSend() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.total = prune(l.total, now.Add(-time.Hour))
	if len(l.total) >= l.lim.TotalPerHour {
		return false
	}
	l.total = append(l.total, now)
	return true
}

// ClientAddr returns the network address a request came from. Behind a
// reverse proxy or tunnel on the same host or private network the peer is
// the proxy, so the address it observed (the last X-Forwarded-For entry) is
// used instead. A header sent by a direct, public peer is never trusted.
func ClientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(last) != nil {
				return last
			}
		}
	}
	return host
}
