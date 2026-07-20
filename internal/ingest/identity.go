package ingest

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Salt holds the rotating secret used to derive visitor identifiers.
//
// It is generated at boot, lives only in memory, is never logged and never
// written to disk. Rotating it is what makes the visitor id non-reversible
// over time: after a rotation, yesterday's hashes cannot be linked to today's
// even by the operator. The cost is that a visitor spanning a rotation counts
// twice in the 7d/30d windows — an unavoidable consequence of refusing to
// persist an identifier.
type Salt struct {
	mu       sync.RWMutex
	cur      []byte
	rotated  time.Time
	interval time.Duration
}

// NewSalt seeds a salt from the system CSPRNG.
func NewSalt(interval time.Duration) (*Salt, error) {
	s := &Salt{interval: interval}
	if err := s.Rotate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Rotate replaces the salt with fresh random bytes.
func (s *Salt) Rotate() error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	s.mu.Lock()
	s.cur = b
	s.rotated = time.Now()
	s.mu.Unlock()
	return nil
}

// RotateIfDue rotates when the interval has elapsed.
func (s *Salt) RotateIfDue(now time.Time) bool {
	s.mu.RLock()
	due := now.Sub(s.rotated) >= s.interval
	s.mu.RUnlock()
	if due {
		_ = s.Rotate()
	}
	return due
}

// VisitorID derives a stable-for-today identifier. The raw IP never leaves
// this function: no metric carries an IP label, by construction.
func (s *Salt) VisitorID(ip netip.Addr, ua, domain string) uint64 {
	s.mu.RLock()
	key := s.cur
	s.mu.RUnlock()

	m := hmac.New(sha256.New, key)
	m.Write([]byte(ip.String()))
	m.Write([]byte{0})
	m.Write([]byte(ua))
	m.Write([]byte{0})
	m.Write([]byte(domain))
	return binary.BigEndian.Uint64(m.Sum(nil)[:8])
}

// ClientIP extracts the caller's address.
//
// X-Forwarded-For is only honoured when trustProxy is set. Trusting it
// unconditionally would let any client forge its own geolocation and, worse,
// evade rate limiting by rotating a header.
func ClientIP(r *http.Request, trustProxy bool) netip.Addr {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Left-most entry is the original client.
			if first, _, ok := strings.Cut(xff, ","); ok {
				xff = first
			}
			if a, err := netip.ParseAddr(strings.TrimSpace(xff)); err == nil {
				return a.Unmap()
			}
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			if a, err := netip.ParseAddr(xr); err == nil {
				return a.Unmap()
			}
		}
	}
	host := r.RemoteAddr
	if ap, err := netip.ParseAddrPort(host); err == nil {
		return ap.Addr().Unmap()
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap()
	}
	return netip.Addr{}
}

// rateKey buckets an address to a prefix, so rate limiting survives a client
// walking its own /24 or IPv6 /64.
func rateKey(ip netip.Addr) string {
	if !ip.IsValid() {
		return "invalid"
	}
	bits := 24
	if ip.Is6() {
		bits = 64
	}
	p, err := ip.Prefix(bits)
	if err != nil {
		return ip.String()
	}
	return p.String()
}
