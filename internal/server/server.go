// Package server implements the Gravatar-compatible HTTP surface (SPEC §4)
// and the request pipeline of SPEC §5.
package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/liueic/avatar/internal/avatar"
	"github.com/liueic/avatar/internal/cache"
	"github.com/liueic/avatar/internal/cdn"
	"github.com/liueic/avatar/internal/config"
	"github.com/liueic/avatar/internal/fetcher"
	"github.com/liueic/avatar/internal/metrics"
	"github.com/liueic/avatar/internal/moderate"
)

// Server aggregates every dependency of the public HTTP surface.
type Server struct {
	cfg     config.Config
	store   *cache.Store
	blobs   *cache.Blobs
	avatars *avatar.Generator
	fetch   *fetcher.Fetcher
	upTB    *fetcher.TokenBucket
	queue   *moderate.Service
	policy  *cdn.Policy
	log     *slog.Logger
	reg     *metrics.Registry
	sf      singleflight.Group

	global *limiterSet
	perIP  *limiterSet

	// touch rate-limits SQLite sliding-TTL updates on hot entries: a hit
	// only writes when the last recorded touch is older than touchInterval.
	touchMu sync.Mutex
	touch   map[string]int64

	// retryAt throttles async re-fetches of network-error negatives.
	retryMu sync.Mutex
	retryAt map[string]int64

	// scaledCache memoizes resized approved images for common small sizes.
	scaled *avatar.LRU
	// scaledDisk persists scaled variants across restarts (LRU is only the
	// first line of caching; the cleaner prunes the disk layer).
	scaledDisk *scaledDiskCache
}

// Deps bundles constructor dependencies.
type Deps struct {
	Cfg     config.Config
	Store   *cache.Store
	Blobs   *cache.Blobs
	Avatars *avatar.Generator
	Fetch   *fetcher.Fetcher
	UpTB    *fetcher.TokenBucket
	Queue   *moderate.Service
	Policy  *cdn.Policy
	Log     *slog.Logger
	Reg     *metrics.Registry
}

// New assembles the server.
func New(d Deps) (*Server, error) {
	s := &Server{
		cfg:     d.Cfg,
		store:   d.Store,
		blobs:   d.Blobs,
		avatars: d.Avatars,
		fetch:   d.Fetch,
		upTB:    d.UpTB,
		queue:   d.Queue,
		policy:  d.Policy,
		log:     d.Log,
		reg:     d.Reg,
		touch:   make(map[string]int64, 1024),
		retryAt: make(map[string]int64, 1024),
		scaled:  avatar.NewLRU(1024),
	}
	if d.Cfg.RateLimit.Enabled {
		s.global = newLimiterSet(d.Cfg.RateLimit.GlobalRPS, d.Cfg.RateLimit.GlobalBurst, 4, d.Reg)
		s.perIP = newLimiterSet(d.Cfg.RateLimit.PerIPRPS, d.Cfg.RateLimit.PerIPBurst, 200_000, d.Reg)
	}
	sd, err := newScaledDiskCache(d.Cfg.Cache.Dir)
	if err != nil {
		return nil, fmt.Errorf("server: scaled disk cache: %w", err)
	}
	s.scaledDisk = sd
	return s, nil
}

// Handler builds the public mux: Gravatar routes + health + optional metrics.
// Note: "/avatar/{hash}.png" needs no dedicated route — the extension arrives
// inside the {hash} segment and the handler strips it (SPEC §4.1).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /avatar/{hash}", s.handleAvatar)
	mux.HandleFunc("GET /{$}", s.handleLanding)
	// Gravatar compatibility: /{hash} without the /avatar prefix (SPEC §4.1).
	mux.HandleFunc("GET /{hash}", s.handleAvatar)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	if s.cfg.Metrics.Enabled {
		mux.HandleFunc("GET /metrics", s.handleMetrics)
	}
	return s.accessLog(s.secureHeaders(mux))
}

// --- middleware ---

func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'")
		h.Set("Access-Control-Allow-Origin", "*") // Gravatar parity (SPEC §16.2)
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// accessLog logs a privacy-whitelisted line per request: hash, status,
// latency — never the query string, never the full client IP unless enabled
// (SPEC §11).
func (s *Server) accessLog(next http.Handler) http.Handler {
	if !s.cfg.Log.AccessLog {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		path := r.URL.Path // path only: no query string ever reaches the log
		attrs := []any{
			"method", r.Method,
			"path", path,
			"status", rec.status,
			"latency_ms", time.Since(start).Milliseconds(),
		}
		if ip := clientIPForLog(r, s.cfg.Log.ClientIPLog); ip != "" {
			attrs = append(attrs, "client_ip", ip)
		}
		switch {
		case rec.status >= 500:
			s.log.Error("request", attrs...)
		case rec.status >= 400:
			s.log.Warn("request", attrs...)
		default:
			s.log.Info("request", attrs...)
		}
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	s.policy.ApplyCacheControl(w.Header(), cdn.KindNoStore)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz reports readiness (model loaded / store reachable). The avatar
// path never 5xxes during warmup — it serves defaults (SPEC §14).
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	s.policy.ApplyCacheControl(w.Header(), cdn.KindNoStore)
	if !s.queue.ModeratorReady() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("moderation engine loading"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.policy.ApplyCacheControl(w.Header(), cdn.KindNoStore)
	s.reg.Gauge("avater_queue_depth", "Moderation queue length", nil, float64(s.queue.Depth()))
	s.reg.Gauge("avater_ratelimit_tracked_keys", "Per-IP rate-limit buckets tracked", nil, float64(s.perIP.Len()))
	body := s.reg.Render()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write(body)
}
