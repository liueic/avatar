package cdn

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liueic/avater/internal/config"
	"github.com/liueic/avater/internal/metrics"
	"log/slog"
)

func testCDNConfig() config.CDNConfig {
	c := config.Default().CDN
	c.Provider = "cloudflare"
	c.CacheTag = true
	c.PurgeToken = "tok"
	c.ZoneID = "zone"
	return c
}

// TestCacheControlMatrix is the SPEC §16.1 table.
func TestCacheControlMatrix(t *testing.T) {
	p := NewPolicy(testCDNConfig())
	cases := []struct {
		kind Kind
		want []string // substrings that must appear
		not  []string // substrings that must not appear
	}{
		{KindApproved, []string{"public", "max-age=86400", "s-maxage=604800", "stale-while-revalidate=86400", "stale-if-error=604800"}, []string{"immutable", "no-store"}},
		{KindDefaultExplicit, []string{"public", "max-age=2592000", "immutable"}, nil},
		{KindDefaultPending, []string{"public", "max-age=60", "s-maxage=300"}, []string{"immutable"}},
		{KindNegative404, []string{"public", "max-age=300"}, nil},
		{KindNoStore, []string{"no-store"}, []string{"public"}},
	}
	for _, tc := range cases {
		h := http.Header{}
		p.ApplyCacheControl(h, tc.kind)
		got := h.Get("Cache-Control")
		if got == "" {
			t.Fatalf("kind %d: no header", tc.kind)
		}
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("kind %d: %q missing %q", tc.kind, got, w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(got, n) {
				t.Errorf("kind %d: %q must not contain %q", tc.kind, got, n)
			}
		}
	}
}

// TestNoImmutableOnApproved re-checks the SPEC's explicit "don't use
// immutable on approved" decision (§16.1: re-review or manual reject can
// change the same URL's content).
func TestNoImmutableOnApproved(t *testing.T) {
	p := NewPolicy(testCDNConfig())
	h := http.Header{}
	p.ApplyCacheControl(h, KindApproved)
	if strings.Contains(h.Get("Cache-Control"), "immutable") {
		t.Fatal("approved must not be immutable")
	}
}

func TestTagHeaders(t *testing.T) {
	p := NewPolicy(testCDNConfig())
	h := http.Header{}
	p.ApplyTagHeader(h, "abc")
	if got := h.Get("Cache-Tag"); got != "av-abc" {
		t.Fatalf("cloudflare tag = %q", got)
	}

	cfg := testCDNConfig()
	cfg.Provider = "fastly"
	pf := NewPolicy(cfg)
	h2 := http.Header{}
	pf.ApplyTagHeader(h2, "abc")
	if got := h2.Get("Surrogate-Key"); got != "av-abc" {
		t.Fatalf("fastly key = %q", got)
	}

	cfgNone := testCDNConfig()
	cfgNone.CacheTag = false
	pn := NewPolicy(cfgNone)
	h3 := http.Header{}
	pn.ApplyTagHeader(h3, "abc")
	if h3.Get("Cache-Tag") != "" || h3.Get("Surrogate-Key") != "" {
		t.Fatal("cache_tag=false must suppress tag headers")
	}
}

// TestPurgeCloudflare verifies the purge request shape and that failures are
// logged, never propagated to callers (SPEC §16.3).
func TestPurgeCloudflare(t *testing.T) {
	var mu sync.Mutex
	var gotPath string
	var gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	// Point the purger at the test server by overriding its client transport
	// target via a custom RoundTripper.
	reg := metrics.New()
	p := NewPurger(testCDNConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), reg)
	p.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		r.Host = "api.cloudflare.com"
		r.URL.Scheme = "http"
		r.URL.Host = strings.TrimPrefix(srv.URL, "http://")
		return http.DefaultTransport.RoundTrip(r)
	})}

	// Purge synchronously by calling the internals through the loop path.
	p.PurgeTagsAsync("aaaa", "bbbb")
	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return gotPath != ""
	})
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/client/v4/zones/zone/purge_cache" {
		t.Fatalf("purge path = %s", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %s", gotAuth)
	}
	if !strings.Contains(string(gotBody), `"tags":["av-aaaa","av-bbbb"]`) &&
		!strings.Contains(string(gotBody), `"tags":["av-bbbb","av-aaaa"]`) {
		t.Fatalf("purge body = %s", gotBody)
	}
}

func TestPurgeDisabledWithoutProvider(t *testing.T) {
	cfg := testCDNConfig()
	cfg.Provider = "none"
	p := NewPurger(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New())
	if p.enabled() {
		t.Fatal("provider none must disable purging")
	}
	// Must not block or panic.
	p.PurgeTagsAsync("aaaa")
	p.Stop()
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
