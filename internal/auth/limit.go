package auth

import (
	"sync"
	"time"
)

const (
	maxFails   = 5
	failWindow = 15 * time.Minute
)

// limiter locks an IP out for failWindow after maxFails failed sign-ins.
type limiter struct {
	mu sync.Mutex
	m  map[string]*attempts
}

type attempts struct {
	fails int
	first time.Time
}

func newLimiter() *limiter { return &limiter{m: map[string]*attempts{}} }

func (l *limiter) blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	a := l.m[ip]
	return a != nil && a.fails >= maxFails
}

func (l *limiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	a := l.m[ip]
	if a == nil {
		a = &attempts{first: now}
		l.m[ip] = a
	}
	a.fails++
}

func (l *limiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, ip)
}

// prune drops entries whose window has passed, so the map cannot grow without bound.
func (l *limiter) prune(now time.Time) {
	for ip, a := range l.m {
		if now.Sub(a.first) >= failWindow {
			delete(l.m, ip)
		}
	}
}
