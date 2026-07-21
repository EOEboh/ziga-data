package httpapi

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type ipLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	perMin   int
	burst    int
}

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// newIPLimiter builds a per-IP limiter with the default burst of 5.
func newIPLimiter(perMin int) *ipLimiter {
	return newIPLimiterBurst(perMin, 5)
}

// newIPLimiterBurst builds a per-IP limiter with an explicit burst — used by
// the stricter auth limiters (login, password reset).
func newIPLimiterBurst(perMin, burst int) *ipLimiter {
	l := &ipLimiter{visitors: map[string]*visitor{}, perMin: perMin, burst: burst}
	go l.cleanup()
	return l
}

func (l *ipLimiter) get(ip string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.visitors[ip]
	if !ok {
		v = &visitor{limiter: rate.NewLimiter(rate.Limit(float64(l.perMin)/60.0), l.burst)}
		l.visitors[ip] = v
	}
	v.lastSeen = time.Now()
	return v.limiter
}

func (l *ipLimiter) cleanup() {
	for range time.Tick(5 * time.Minute) {
		l.mu.Lock()
		for ip, v := range l.visitors {
			if time.Since(v.lastSeen) > 15*time.Minute {
				delete(l.visitors, ip)
			}
		}
		l.mu.Unlock()
	}
}

func clientIP(r *http.Request) string {
	// Behind the VPS reverse proxy (nginx/caddy) the peer address is the
	// proxy; prefer X-Forwarded-For's first hop when present.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimit enforces the server's shared per-IP limiter, so all wrapped
// endpoints draw from one budget.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.get(clientIP(r)).Allow() {
			httpError(w, http.StatusTooManyRequests, "rate limit exceeded — slow down")
			return
		}
		next.ServeHTTP(w, r)
	})
}
