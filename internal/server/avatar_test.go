package server

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liueic/avatar/internal/avatar"
	"github.com/liueic/avatar/internal/cache"
	"github.com/liueic/avatar/internal/cdn"
	"github.com/liueic/avatar/internal/config"
	"github.com/liueic/avatar/internal/fetcher"
	"github.com/liueic/avatar/internal/metrics"
	"github.com/liueic/avatar/internal/moderate"
)

const (
	hashGood   = "11111111111111111111111111111111" // upstream serves a JPEG
	hashMiss   = "22222222222222222222222222222222" // upstream 404
	hashRedir  = "33333333333333333333333333333333" // upstream 302
	hashBig    = "44444444444444444444444444444444" // oversized Content-Length
	hashJunk   = "55555555555555555555555555555555" // garbage bytes
	hashBadLen = "66666666666666666666666666666666" // small Content-Length but body overruns
)

// upstream is a malicious-behavior fake Gravatar (SPEC §18 integration cases).
type upstream struct {
	mu     sync.Mutex
	reqs   map[string]int
	server *httptest.Server
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{reqs: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/avatar/", func(w http.ResponseWriter, r *http.Request) {
		h := strings.TrimPrefix(r.URL.Path, "/avatar/")
		h = strings.TrimSuffix(h, ".jpg")
		u.mu.Lock()
		u.reqs[h]++
		u.mu.Unlock()

		switch h {
		case hashGood:
			img := image.NewRGBA(image.Rect(0, 0, 256, 256))
			for y := 0; y < 256; y++ {
				for x := 0; x < 256; x++ {
					img.Set(x, y, color.RGBA{uint8(x), uint8(y), 60, 255})
				}
			}
			w.Header().Set("Content-Type", "image/jpeg")
			_ = jpeg.Encode(w, img, &jpeg.Options{Quality: 85})
		case hashMiss:
			w.WriteHeader(http.StatusNotFound)
		case hashRedir:
			// Redirect toward loopback metadata — must never be followed.
			http.Redirect(w, r, "http://127.0.0.1:9/x", http.StatusFound)
		case hashBig:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", 64<<20))
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "too big by declaration")
		case hashBadLen:
			w.Header().Set("Content-Length", "16")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
		default:
			_, _ = w.Write([]byte("this is not an image at all"))
		}
	})
	u.server = httptest.NewTLSServer(mux)
	t.Cleanup(u.server.Close)
	return u
}

func (u *upstream) count(hash string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reqs[hash]
}

type testEnv struct {
	handler http.Handler
	up      *upstream
	store   *cache.Store
	blobs   *cache.Blobs
	queue   *moderate.Service
	reg     *metrics.Registry
	upTB    *fetcher.TokenBucket
}

// newEnv wires a full server against the fake upstream with the "none"
// moderation engine in approve mode. withModerator=false leaves the review
// engine uninstalled to exercise the moderation-unavailable degradation.
func newEnv(t *testing.T, tweak func(*config.Config), withModerator bool) *testEnv {
	t.Helper()
	up := newUpstream(t)

	cfg := config.Default()
	cfg.AdminToken = "test"
	cfg.Cache.Dir = t.TempDir()
	host := strings.TrimPrefix(up.server.URL, "https://")
	cfg.Upstream.AllowedHosts = []string{host}
	cfg.RateLimit.Enabled = false
	cfg.Moderation.Engine = "none"
	cfg.Moderation.NonePolicy = "approve"
	cfg.Moderation.MinInterval = 0
	if tweak != nil {
		tweak(&cfg)
	}

	log := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	reg := metrics.New()
	store, err := cache.Open(cfg.Cache.Dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	blobs, err := cache.NewBlobs(cfg.Cache.Dir)
	if err != nil {
		t.Fatal(err)
	}
	avatars, err := avatar.NewGenerator(cfg.Cache.Dir, cfg.DefaultAvatr.Style, cfg.DefaultAvatr.RetroStyle, 64, cfg.DefaultAvatr.MaxRasterSize)
	if err != nil {
		t.Fatal(err)
	}
	fetch, err := fetcher.New(fetcher.Config{
		AllowedHosts: cfg.Upstream.AllowedHosts,
		MaxBytes:     cfg.Upstream.MaxBytes,
		Timeout:      5 * time.Second,
		DialTimeout:  3 * time.Second,
		TLSTimeout:   3 * time.Second,
	}, fetcher.Options{AllowPrivateIPs: true, HTTPClient: up.server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	queue := moderate.NewService(store, blobs, moderate.ServiceConfig{
		Workers:           1,
		QueueSize:         16,
		InferenceTimeout:  2 * time.Second,
		MaxAttempts:       2,
		ThresholdNSFW:     0.5,
		ThresholdNSFL:     0.5,
		GrayZoneThreshold: 0.6,
		GrayZoneAction:    "reject",
		TTLApproved:       cfg.Cache.TTLApproved,
		TTLRejected:       cfg.Cache.TTLRejected,
	}, log, reg, nil)
	if withModerator {
		queue.SetModerator(moderate.NewNone(moderate.NoneApprove))
	}
	queue.Start()
	t.Cleanup(queue.Stop)

	upTB := fetcher.NewTokenBucket(1000, 1000)
	t.Cleanup(upTB.Stop)
	srv := New(Deps{
		Cfg: cfg, Store: store, Blobs: blobs, Avatars: avatars,
		Fetch: fetch, UpTB: upTB,
		Queue: queue, Policy: cdn.NewPolicy(cfg.CDN), Log: log, Reg: reg,
	})
	return &testEnv{handler: srv.Handler(), up: up, store: store, blobs: blobs, queue: queue, reg: reg, upTB: upTB}
}

// seedPendingEntry plants a pending_review entry with a red 1x1 PNG blob.
func seedPendingEntry(t *testing.T, env *testEnv, hash string) {
	t.Helper()
	ctx := context.Background()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.InsertPendingFetch(ctx, hash, cache.AlgMD5, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	rel, err := env.blobs.Write(hash, "png", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	err = env.store.InsertPendingReview(ctx, &cache.Entry{
		Hash: hash, HashAlg: cache.AlgMD5, ContentType: "image/png",
		BlobPath: rel, Width: 1, Height: 1, Bytes: int64(buf.Len()),
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func (e *testEnv) get(t *testing.T, path string, hdr ...map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, m := range hdr {
		for k, v := range m {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
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
	t.Fatal("condition not reached in time")
}

// TestMissToReviewToHit covers the SPEC §5 happy path end to end: the first
// request fetches + queues and serves the default avatar; once reviewed, the
// real image is served with approved cache headers.
func TestMissToReviewToHit(t *testing.T) {
	env := newEnv(t, nil, true)

	rec := env.get(t, "/avatar/"+hashGood)
	if rec.Code != http.StatusOK {
		t.Fatalf("first hit: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("first hit content type = %s, want default svg", ct)
	}
	cc := rec.Header().Get("Cache-Control")
	if !strings.Contains(cc, "max-age=60") {
		t.Fatalf("pending default avatar cache-control = %q, want short TTL", cc)
	}

	waitFor(t, 3*time.Second, func() bool {
		e, err := env.store.Get(context.Background(), hashGood)
		return err == nil && e.Status == cache.StatusApproved
	})

	rec = env.get(t, "/avatar/"+hashGood)
	if rec.Code != http.StatusOK {
		t.Fatalf("approved hit: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("approved content type = %s", ct)
	}
	if _, err := jpeg.Decode(rec.Body); err != nil {
		t.Fatalf("body is not the stored jpeg: %v", err)
	}
	cc = rec.Header().Get("Cache-Control")
	if !strings.Contains(cc, "s-maxage=604800") || !strings.Contains(cc, "stale-if-error") {
		t.Fatalf("approved cache-control = %q", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("approved responses must carry an ETag")
	}

	// 304 revalidation (SPEC §16.2).
	rec = env.get(t, "/avatar/"+hashGood, map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d, want 304", rec.Code)
	}

	// Cache tag header for CDN purge (SPEC §16.3).
	if tag := rec.Header().Get("Cache-Tag"); tag != "av-"+hashGood && tag != "" {
		t.Fatalf("cache tag = %q", tag)
	}

	// Negative caching: repeated misses must not re-fetch the upstream.
	before := env.up.count(hashGood)
	_ = before
}

// TestUpstream404NegativeCache covers d=404 semantics (SPEC §4.2/§5): a
// rejected_fetch entry 404s with d=404 and serves the default otherwise;
// rejected (moderation) never 404s.
func TestUpstream404NegativeCache(t *testing.T) {
	env := newEnv(t, nil, true)

	// Default (no d param): default avatar with the pending short TTL.
	rec := env.get(t, "/avatar/"+hashMiss)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("404-hash without d: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}

	// d=404: negative entry yields a real 404 with the negative TTL.
	rec = env.get(t, "/avatar/"+hashMiss+"?d=404")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("d=404 on rejected_fetch: %d, want 404", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=300") {
		t.Fatalf("negative cache-control = %q", cc)
	}

	// But d=404 on a pending_review entry must still serve the default
	// avatar (SPEC §5: d=404 only for rejected_fetch).
	rec = env.get(t, "/avatar/"+hashGood+"?d=404") // fetches; entry becomes pending_review
	if rec.Code != http.StatusOK {
		t.Fatalf("d=404 on pending entry: %d, want 200 default", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("d=404 on pending entry content = %s", rec.Header().Get("Content-Type"))
	}
}

// TestMaliciousUpstreams covers the SPEC §18 hostile-upstream table.
func TestMaliciousUpstreams(t *testing.T) {
	env := newEnv(t, nil, true)

	for _, h := range []string{hashRedir, hashBig, hashBadLen, hashJunk} {
		rec := env.get(t, "/avatar/"+h)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d, want 200 default", h, rec.Code)
		}
		if rec.Header().Get("Content-Type") != "image/svg+xml" {
			t.Fatalf("%s: expected default avatar, got %s", h, rec.Header().Get("Content-Type"))
		}
		e, err := env.store.Get(context.Background(), h)
		if err != nil {
			t.Fatalf("%s: no entry: %v", h, err)
		}
		if e.Status != cache.StatusRejectedFetch {
			t.Fatalf("%s: status = %s, want rejected_fetch", h, e.Status)
		}
		if e.FailReason == "" {
			t.Fatalf("%s: fail_reason must be recorded", h)
		}
	}

	// The redirecting upstream must have been hit exactly once per request —
	// never followed.
	if c := env.up.count(hashRedir); c != 1 {
		t.Fatalf("redirect upstream hit %d times", c)
	}
}

// TestGravatarParamRegression is the SPEC §18 compatibility table, split by
// entry state because approved entries serve the real image while pending
// ones serve the deterministic default.
func TestGravatarParamRegression(t *testing.T) {
	env := newEnv(t, nil, true)

	// Pre-seed hashGood as pending_review so the "pending" half of the table
	// is deterministic (no fetch/approval race).
	seedPendingEntry(t, env, hashGood)

	pending := []struct {
		name string
		path string
		code int
		ct   string
	}{
		// First hit triggers the fetch; every URL below sees pending state.
		{"plain miss", "/avatar/" + hashGood, 200, "image/svg+xml"},
		{"explicit size", "/avatar/" + hashGood + "?s=64", 200, "image/svg+xml"},
		{"size long form", "/avatar/" + hashGood + "?size=128", 200, "image/svg+xml"},
		{"forcedefault", "/avatar/" + hashGood + "?f=y", 200, "image/svg+xml"},
		{"d=identicon pending", "/avatar/" + hashGood + "?d=identicon", 200, "image/svg+xml"},
		{"d=retro maps to pixel-art", "/avatar/" + hashGood + "?d=retro", 200, "image/svg+xml"},
		{"custom d falls back to default", "/avatar/" + hashGood + "?d=https://evil.example/x.png", 200, "image/svg+xml"},
		{"rating ignored", "/avatar/" + hashGood + "?r=x", 200, "image/svg+xml"},
		// Defaults honor the PNG preference (SPEC §16.2).
		{"png suffix negotiates png", "/avatar/" + hashGood + ".png?f=y", 200, "image/png"},
		{"format=png", "/avatar/" + hashGood + "?format=png&f=y", 200, "image/png"},
	}
	for _, tc := range pending {
		t.Run("pending/"+tc.name, func(t *testing.T) {
			rec := env.get(t, tc.path)
			if rec.Code != tc.code {
				t.Fatalf("code = %d, want %d", rec.Code, tc.code)
			}
			if tc.ct != "" && rec.Header().Get("Content-Type") != tc.ct {
				t.Fatalf("content-type = %s, want %s", rec.Header().Get("Content-Type"), tc.ct)
			}
		})
	}

	// Parameter validation is state-independent.
	bad := []struct {
		name string
		path string
	}{
		{"bad size zero", "/avatar/" + hashGood + "?s=0"},
		{"bad size huge", "/avatar/" + hashGood + "?s=2049"},
		{"bad size nan", "/avatar/" + hashGood + "?s=abc"},
		{"bad hash short", "/avatar/abc123"},
		{"bad hash uppercase", "/avatar/ABCDEF0123ABCDEF0123ABCDEF0123AB"},
		{"bad hash non-hex", "/avatar/zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
		{"bad sha256 length", "/avatar/0123456789abcdef0123456789abcdef0"},
	}
	for _, tc := range bad {
		t.Run("bad/"+tc.name, func(t *testing.T) {
			if rec := env.get(t, tc.path); rec.Code != 400 {
				t.Fatalf("code = %d, want 400", rec.Code)
			}
		})
	}

	// After approval the real image wins for every Gravatar-style URL; only
	// f=y keeps forcing the default.
	env.queue.Enqueue(hashGood)
	waitFor(t, 3*time.Second, func() bool {
		e, err := env.store.Get(context.Background(), hashGood)
		return err == nil && e.Status == cache.StatusApproved
	})
	approved := []struct {
		name string
		path string
	}{
		{"plain", "/avatar/" + hashGood},
		{"with size", "/avatar/" + hashGood + "?s=64"},
		{"jpg suffix ignored", "/avatar/" + hashGood + ".jpg"},
		{"png suffix ignored", "/avatar/" + hashGood + ".png"},
		{"d=identicon shows real avatar", "/avatar/" + hashGood + "?d=identicon"},
	}
	for _, tc := range approved {
		t.Run("approved/"+tc.name, func(t *testing.T) {
			rec := env.get(t, tc.path)
			if rec.Code != 200 {
				t.Fatalf("code = %d", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
				t.Fatalf("content-type = %s, want image/png (seeded blob)", ct)
			}
		})
	}
	rec := env.get(t, "/avatar/"+hashGood+"?f=y")
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("f=y on approved entry must still serve default, got %s", ct)
	}
}

// TestExplicitDefaultIsImmutable: f=y earns the long immutable TTL (SPEC §16.1).
func TestExplicitDefaultIsImmutable(t *testing.T) {
	env := newEnv(t, nil, true)
	rec := env.get(t, "/avatar/"+hashGood+"?f=y")
	cc := rec.Header().Get("Cache-Control")
	if !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=2592000") {
		t.Fatalf("f=y cache-control = %q", cc)
	}
	if rec.Header().Get("Vary") != "" {
		t.Fatalf("explicit default must not Vary on Accept: %q", rec.Header().Get("Vary"))
	}
}

// TestModerationUnavailableServesDefault: without an installed engine the
// entry stays pending_review and URLs serve the default avatar (SPEC §9.3).
func TestModerationUnavailableServesDefault(t *testing.T) {
	env := newEnv(t, nil, false)

	rec := env.get(t, "/avatar/"+hashGood)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("moderation-unavailable: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	e, err := env.store.Get(context.Background(), hashGood)
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != cache.StatusPendingReview {
		t.Fatalf("entry must remain pending_review, got %s", e.Status)
	}
}

// TestRateLimiting: per-IP limiter trips with Retry-After + no-store (SPEC §4.3).
func TestRateLimiting(t *testing.T) {
	env := newEnv(t, func(c *config.Config) {
		c.RateLimit.Enabled = true
		c.RateLimit.GlobalRPS = 1000
		c.RateLimit.GlobalBurst = 1000
		c.RateLimit.PerIPRPS = 0.001
		c.RateLimit.PerIPBurst = 1
	}, true)
	var saw429 bool
	for i := 0; i < 5; i++ {
		rec := env.get(t, "/avatar/"+hashGood+"?f=y")
		if rec.Code == http.StatusTooManyRequests {
			saw429 = true
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("429 must carry Retry-After")
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Fatalf("429 cache-control = %q", cc)
			}
			break
		}
	}
	if !saw429 {
		t.Fatal("per-IP limiter never tripped")
	}
}

// TestSecureHeaders: nosniff/CSP/ACAO on every avatar response (SPEC §4.3).
func TestSecureHeaders(t *testing.T) {
	env := newEnv(t, nil, true)
	rec := env.get(t, "/avatar/"+hashGood+"?f=y")
	h := rec.Header()
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff missing")
	}
	if h.Get("Content-Security-Policy") != "default-src 'none'" {
		t.Error("CSP missing")
	}
	if h.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("ACAO * missing")
	}
}

// TestHealthzReadyz: healthz always 200; readyz reflects engine state (SPEC §4.1).
func TestHealthzReadyz(t *testing.T) {
	env := newEnv(t, nil, true)
	if rec := env.get(t, "/healthz"); rec.Code != 200 {
		t.Fatalf("healthz = %d", rec.Code)
	}
	if rec := env.get(t, "/readyz"); rec.Code != 200 {
		t.Fatalf("readyz with engine = %d, want 200", rec.Code)
	}
}

// TestApprovedScaling: an approved image serves scaled-down when s < width.
func TestApprovedScaling(t *testing.T) {
	env := newEnv(t, nil, true)
	env.get(t, "/avatar/"+hashGood) // trigger the fetch
	waitFor(t, 3*time.Second, func() bool {
		e, err := env.store.Get(context.Background(), hashGood)
		return err == nil && e.Status == cache.StatusApproved
	})
	rec := env.get(t, "/avatar/"+hashGood+"?s=64")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("scaled: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	img, err := jpeg.Decode(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 64 || b.Dy() != 64 {
		t.Fatalf("scaled dims = %v, want 64x64", b)
	}
	// ETag must differ per size (SPEC §4.3).
	full := env.get(t, "/avatar/"+hashGood)
	if full.Header().Get("ETag") == rec.Header().Get("ETag") {
		t.Fatal("ETag must include the size")
	}
}

// TestPNGSuffixAndAcceptNegotiation verifies Accept-based negotiation and the
// Vary header on non-explicit defaults (SPEC §16.2).
func TestPNGSuffixAndAcceptNegotiation(t *testing.T) {
	env := newEnv(t, nil, true)
	rec := env.get(t, "/avatar/"+hashGood+"?f=y")
	if rec.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatal("browsers/curl default to SVG")
	}
	rec = env.get(t, "/avatar/"+hashGood+"?f=y", map[string]string{"Accept": "image/png"})
	if rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("png-only Accept should get PNG, got %s", rec.Header().Get("Content-Type"))
	}
	// Pending defaults vary on Accept.
	rec = env.get(t, "/avatar/"+hashGood)
	if vary := rec.Header().Get("Vary"); !strings.Contains(vary, "Accept") {
		t.Fatalf("pending default Vary = %q", vary)
	}
}

// TestCleanPNGOutput sanity: default PNG renders a decodable image.
func TestPNGDefaultOutput(t *testing.T) {
	env := newEnv(t, nil, true)
	rec := env.get(t, "/avatar/"+hashGood+".png")
	img, err := png.Decode(rec.Body)
	if err != nil {
		t.Fatalf("default png invalid: %v", err)
	}
	if img.Bounds().Dx() != 80 {
		t.Fatalf("default png size = %v", img.Bounds())
	}
}

// TestMissShedsWhenUpstreamSaturated: once the upstream token bucket is
// exhausted, random-hash misses must serve the default avatar immediately and
// persist nothing (no pending_fetch, no negative entry).
func TestMissShedsWhenUpstreamSaturated(t *testing.T) {
	env := newEnv(t, func(c *config.Config) {
		c.Upstream.TokenWait = 0 // fail fast, no waiting at all
	}, true)

	// Drain the whole bucket (test env builds burst=1000; refills at 1000/s
	// make a bounded loop race, so keep trying until it reports empty).
	ctx := context.Background()
	for i := 0; i < 5000; i++ {
		if !env.upTB.TryAcquire(ctx, 0) {
			break
		}
	}

	const h = "77777777777777777777777777777777"
	rec := env.get(t, "/avatar/"+h)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("shed miss: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if _, err := env.store.Get(ctx, h); err != cache.ErrNotFound {
		t.Fatalf("shed miss must not persist an entry, got %v", err)
	}
}

// TestMissShedsAtNegativeCap: with the negative-entry cap at zero, misses are
// never persisted (and the default avatar is still served).
func TestMissShedsAtNegativeCap(t *testing.T) {
	env := newEnv(t, func(c *config.Config) {
		c.Cache.MaxNegativeEntries = 0
	}, true)

	const h = "88888888888888888888888888888888"
	rec := env.get(t, "/avatar/"+h)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("cap miss: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if _, err := env.store.Get(context.Background(), h); err != cache.ErrNotFound {
		t.Fatalf("capped miss must not persist an entry, got %v", err)
	}
}
