package web

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Keep source throttles bounded without evicting active limits under load.
type signInLimit struct {
	mu      sync.Mutex
	sources map[netip.Addr]*rate.Limiter
	trusted []netip.Prefix
}

func (l *signInLimit) source(r *http.Request) netip.Addr {
	peer, _ := netip.ParseAddrPort(r.RemoteAddr)
	ip := peer.Addr().Unmap()
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		if !slices.ContainsFunc(l.trusted, func(p netip.Prefix) bool { return p.Contains(ip) }) {
			break
		}
		next, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		ip = next.Unmap()
	}
	// IPv6 privacy addresses on one network share a limit.
	if ip.Is6() {
		ip = netip.PrefixFrom(ip, 64).Masked().Addr()
	}
	return ip
}

func (l *signInLimit) allow(r *http.Request) bool {
	ip, now := l.source(r), time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sources == nil {
		l.sources = make(map[netip.Addr]*rate.Limiter)
	}
	if l.sources[ip] == nil {
		if len(l.sources) >= 4096 {
			for key, limit := range l.sources {
				if limit.TokensAt(now) == float64(limit.Burst()) {
					delete(l.sources, key)
				}
			}
			if len(l.sources) >= 4096 {
				return false
			}
		}
		l.sources[ip] = rate.NewLimiter(2, 10)
	}
	return l.sources[ip].AllowN(now, 1)
}
