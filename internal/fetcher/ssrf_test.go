package fetcher

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

// TestCheckIPReservedRanges is the SPEC §18 table-driven SSRF test: every
// reserved/dangerous range must be rejected; only public addresses pass.
func TestCheckIPReservedRanges(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		// loopback
		{"127.0.0.1", true},
		{"127.8.8.8", true},
		{"::1", true},
		// link-local incl. cloud metadata
		{"169.254.169.254", true},
		{"169.254.0.1", true},
		{"fe80::1", true},
		// RFC1918
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		{"192.168.0.0", true},
		// CGNAT
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		// unspecified / this-network
		{"0.0.0.0", true},
		{"0.1.2.3", true},
		// broadcast / reserved
		{"255.255.255.255", true},
		{"240.0.0.1", true},
		{"250.1.2.3", true},
		// multicast
		{"224.0.0.1", true},
		{"239.255.255.255", true},
		{"ff02::1", true},
		// documentation
		{"192.0.2.1", true},
		{"198.51.100.7", true},
		{"203.0.113.9", true},
		{"2001:db8::1", true},
		// benchmarking
		{"198.18.0.1", true},
		{"198.19.255.255", true},
		// IETF assignments / NAT64 / discard
		{"192.0.0.8", true},
		{"64:ff9b::7f00:1", true},
		{"100::1", true},
		// ULA
		{"fd00::1", true},
		{"fc00::abcd", true},
		// IPv4-mapped wrapping a private address
		{"::ffff:10.0.0.1", true},
		{"::ffff:192.168.0.1", true},
		{"::ffff:127.0.0.1", true},

		// public addresses must pass
		{"151.101.1.100", false},
		{"8.8.8.8", false},
		{"104.16.132.229", false},
		{"2606:4700:4700::1111", false},
		{"2001:4860:4860::8888", false},
		{"::ffff:151.101.1.100", false}, // mapped public
	}
	for _, tc := range cases {
		err := CheckIP(netip.MustParseAddr(tc.ip))
		if tc.blocked && err == nil {
			t.Errorf("CheckIP(%s) = allowed, want blocked", tc.ip)
		}
		if !tc.blocked && err != nil {
			t.Errorf("CheckIP(%s) = %v, want allowed", tc.ip, err)
		}
	}
}

func TestCheckHostWhitelist(t *testing.T) {
	f, err := New(Config{
		AllowedHosts: []string{"secure.gravatar.com", "www.gravatar.com"},
		MaxBytes:     1 << 20,
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !f.CheckHost("secure.gravatar.com") {
		t.Error("exact whitelist host must pass")
	}
	if f.CheckHost("evil.gravatar.com") {
		t.Error("subdomain must be rejected (no wildcard)")
	}
	if f.CheckHost("gravatar.com") {
		t.Error("parent domain must be rejected")
	}
	if f.CheckHost("secure.gravatar.com.evil.io") {
		t.Error("suffix trick must be rejected")
	}
	if !f.CheckHost("SECURE.GRAVATAR.COM.") {
		t.Error("case/trailing-dot normalized host must pass")
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New(Config{AllowedHosts: nil, MaxBytes: 100}, Options{}); err == nil {
		t.Error("empty whitelist must be rejected")
	}
	if _, err := New(Config{AllowedHosts: []string{"a.com"}, MaxBytes: 0}, Options{}); err == nil {
		t.Error("zero max bytes must be rejected")
	}
}

func TestTokenBucketTryAcquire(t *testing.T) {
	tb := NewTokenBucket(1000, 2)
	defer tb.Stop()
	ctx := context.Background()

	if !tb.TryAcquire(ctx, 0) {
		t.Fatal("fresh bucket must grant a token immediately")
	}
	if !tb.TryAcquire(ctx, 0) {
		t.Fatal("second token must be granted (burst=2)")
	}
	if tb.TryAcquire(ctx, 0) {
		t.Fatal("exhausted bucket with zero wait must fail fast")
	}
	if tb.TryAcquire(ctx, time.Millisecond) {
		t.Fatal("1ms wait against a 1s-interval bucket must fail")
	}
}

func TestFetchRejectsWithoutBucketToken(t *testing.T) {
	// The caller now owns pacing; Fetch itself must not block.
	f, err := New(Config{AllowedHosts: []string{"secure.gravatar.com"}, MaxBytes: 1024}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// No token bucket passed in anymore — a request against a dead network
	// fails on its own context, not on bucket wait.
	if _, err := f.Fetch(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 64); err == nil {
		t.Fatal("expected network error")
	}
}
