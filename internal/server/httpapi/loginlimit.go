package httpapi

import (
	"math"
	"sync"
	"time"
)

// loginGate bounds unauthenticated password verification (design §4.3 实现修订
// 2026-09-20).
//
// The verification itself is deliberately expensive — argon2id at 64 MiB per
// attempt (security/password.go) — which makes POST /api/login the cheapest way
// to exhaust the server's memory: N concurrent requests cost N×64 MiB, and no
// authentication is needed to start them. The per-IP blacklist cannot bound
// that on its own: it is identity-based, and an attacker with many source
// addresses (or, before the XFF fix, with a spoofable header) has an unlimited
// supply of identities.
//
// So this gate is deliberately identity-independent:
//
//   - a token bucket caps how many verifications may START per second (CPU), and
//   - an in-flight cap bounds how many run at the same time (memory).
//
// Cost, stated plainly: a sustained flood can make a legitimate operator's
// login wait with 429. That is the price of an absolute memory bound, and it is
// bounded in time (the bucket refills) rather than permanent. An operator who
// cannot wait has the LAN/loopback address and the admin CLI as escape hatches,
// neither of which goes through this handler.
type loginGate struct {
	mu       sync.Mutex
	tokens   float64
	last     time.Time
	inflight int
	// now is nil in production (time.Now) and set by tests.
	now func() time.Time
}

const (
	// loginGateBurst is how many verifications may start back-to-back; a human
	// typing a password never comes close, a script does.
	loginGateBurst = 30.0
	// loginGatePerSecond is the sustained verification rate. Four cores can
	// comfortably run 10×64 MiB argon2id per second; beyond that the CPU cost
	// of one login starts to hurt the rest of the panel.
	loginGatePerSecond = 10.0
	// loginGateMaxInflight is the memory bound: 4 × 64 MiB KDF working set.
	loginGateMaxInflight = 4
)

func (g *loginGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// tryAcquire reserves one verification slot. false means "answer 429 and do no
// work at all": no body read, no database query, no key derivation.
func (g *loginGate) tryAcquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	if g.last.IsZero() {
		g.tokens = loginGateBurst
	} else if elapsed := now.Sub(g.last).Seconds(); elapsed > 0 {
		g.tokens = math.Min(loginGateBurst, g.tokens+elapsed*loginGatePerSecond)
	}
	g.last = now
	if g.inflight >= loginGateMaxInflight || g.tokens < 1 {
		return false
	}
	g.tokens--
	g.inflight++
	return true
}

// release returns the in-flight slot. The token is NOT returned: the rate limit
// counts attempts, not successes.
func (g *loginGate) release() {
	g.mu.Lock()
	if g.inflight > 0 {
		g.inflight--
	}
	g.mu.Unlock()
}
