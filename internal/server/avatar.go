package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"

	"github.com/liueic/avatar/internal/cache"
	"github.com/liueic/avatar/internal/cdn"
	"github.com/liueic/avatar/internal/fetcher"
	"github.com/liueic/avatar/internal/validate"
)

// fetchBusy is a transient miss outcome: the upstream budget or the negative
// entry cap is exhausted, so the request is served the default avatar with
// nothing persisted. It never reaches the database (abuse resilience — see
// the rate-limit notes in README).
const fetchBusy = cache.Status("busy")

const (
	// DefaultSize is the Gravatar default size parameter.
	DefaultSize = 80
	// MaxSize matches Gravatar's upper bound (SPEC §4.2).
	MaxSize = 2048
	// defaultAvatarRev is the rev component of default-avatar ETags; bump it
	// when the default style or rendering pipeline changes.
	defaultAvatarRev = "d2"
)

// parsedRequest carries the normalized Gravatar parameters of one request.
type parsedRequest struct {
	hash      string
	size      int
	d         string // raw d/default value: "", "404", "identicon", "retro", other
	forceDef  bool
	formatPNG bool // explicit .png suffix or ?format=png
}

// handleAvatar implements GET /avatar/{hash} (and /{hash}).
func (s *Server) handleAvatar(w http.ResponseWriter, r *http.Request) {
	req, err := parseRequest(r)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "bad request", "bad_request")
		return
	}

	if s.global != nil && !s.global.Allow("global") {
		s.failRetryAfter(w)
		return
	}
	if s.perIP != nil && !s.perIP.Allow(clientKey(r)) {
		s.failRetryAfter(w)
		return
	}

	// f=y short-circuits everything: the deterministic default is the answer
	// regardless of cache state (SPEC §4.2).
	if req.forceDef {
		s.serveDefault(w, r, req, true)
		return
	}

	s.serveEntry(w, r, req)
}

// parseRequest extracts and validates hash + query parameters (SPEC §4.1/4.2).
// The .jpg/.png suffix is stripped (SPEC §4.1 "忽略并统一处理"); a .png suffix
// additionally records the client's PNG preference for default avatars
// (URL-based format negotiation, SPEC §16.2 — approved images always serve
// their stored format).
func parseRequest(r *http.Request) (*parsedRequest, error) {
	req := &parsedRequest{size: DefaultSize}
	hash := r.PathValue("hash")
	switch {
	case strings.HasSuffix(hash, ".png"):
		req.formatPNG = true
		hash = strings.TrimSuffix(hash, ".png")
	case strings.HasSuffix(hash, ".jpeg"):
		hash = strings.TrimSuffix(hash, ".jpeg")
	case strings.HasSuffix(hash, ".jpg"):
		hash = strings.TrimSuffix(hash, ".jpg")
	}
	if !cache.ValidateHash(hash) {
		return nil, fmt.Errorf("invalid hash")
	}
	req.hash = hash

	q := r.URL.Query()

	if v := firstNonEmpty(q.Get("s"), q.Get("size")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxSize {
			return nil, fmt.Errorf("invalid size")
		}
		req.size = n
	}
	req.d = firstNonEmpty(q.Get("d"), q.Get("default"))

	for _, k := range []string{"f", "forcedefault"} {
		switch q.Get(k) {
		case "y", "Y", "1", "yes":
			req.forceDef = true
		}
	}
	if q.Get("format") == "png" {
		req.formatPNG = true
	}
	return req, nil
}

// serveEntry performs the cache lookup and state-machine branching of SPEC §5.
func (s *Server) serveEntry(w http.ResponseWriter, r *http.Request, req *parsedRequest) {
	ctx := r.Context()

	entry, err := s.store.Get(ctx, req.hash)
	if errors.Is(err, cache.ErrNotFound) {
		s.handleMiss(w, r, req)
		return
	}
	if err != nil {
		// Storage hiccup: degrade to the default avatar, never 5xx (SPEC §4.3).
		s.log.Error("store lookup failed", "hash", req.hash, "err", err)
		s.serveDefault(w, r, req, false)
		return
	}

	switch entry.Status {
	case cache.StatusApproved:
		s.serveApproved(w, r, req, entry)
	case cache.StatusRejectedFetch:
		if req.d == "404" {
			// Only "upstream has no such avatar" yields a 404 (SPEC §5).
			s.serve404(w)
			return
		}
		s.serveDefault(w, r, req, false)
	case cache.StatusPendingReview, cache.StatusRejected:
		s.serveDefault(w, r, req, false)
	case cache.StatusPendingFetch:
		// A fetch is nominally in flight but no singleflight group covers us
		// (crashed process leftover). Ride along with the fetch flow.
		s.handleMiss(w, r, req)
	default:
		s.serveDefault(w, r, req, false)
	}
}

// handleMiss runs the single-flight fetch for an unknown hash and serves the
// resulting state (SPEC §5.2).
func (s *Server) handleMiss(w http.ResponseWriter, r *http.Request, req *parsedRequest) {
	res, _, _ := s.sf.Do(req.hash, func() (any, error) {
		return s.fetchAndStore(r.Context(), req.hash), nil
	})
	outcome, _ := res.(cache.Status)

	if outcome == fetchBusy {
		// Under load-shedding: default avatar, no state, d=404 does not 404
		// (upstream availability is simply unknown).
		s.serveDefault(w, r, req, false)
		return
	}

	switch outcome {
	case cache.StatusApproved:
		// Practically unreachable on a fresh miss; handled for completeness.
		entry, err := s.store.Get(r.Context(), req.hash)
		if err == nil && entry.Status == cache.StatusApproved {
			s.serveApproved(w, r, req, entry)
			return
		}
		s.serveDefault(w, r, req, false)
	case cache.StatusRejectedFetch:
		if req.d == "404" {
			s.serve404(w)
			return
		}
		s.serveDefault(w, r, req, false)
	default:
		// pending_review / pending_fetch: real image queued for review.
		s.serveDefault(w, r, req, false)
	}
}

// fetchAndStore executes the miss path: fetch from the whitelisted upstream,
// validate, persist, enqueue for review. It returns the resulting status.
func (s *Server) fetchAndStore(ctx context.Context, hash string) cache.Status {
	alg := cache.AlgForHash(hash)
	now := time.Now().Unix()
	failCount := 0

	// Abuse gate 1: upstream pacing. Consume the token up front with a
	// bounded wait; when the bucket is saturated we shed load immediately
	// instead of pinning connections for the whole fetch timeout.
	if !s.upTB.TryAcquire(ctx, s.cfg.Upstream.TokenWait) {
		s.reg.Counter("avater_upstream_requests_total", "Upstream fetches by outcome", map[string]string{"outcome": "busy"}, 1)
		return fetchBusy
	}
	// Abuse gate 2: negative-entry growth cap. Random-hash floods stop
	// persisting (and stop fetching) once too many unexpired negatives exist.
	if max := s.cfg.Cache.MaxNegativeEntries; max >= 0 {
		if n, err := s.store.NegativeCount(ctx); err == nil && n >= max {
			s.reg.Counter("avater_negative_cap_skips_total", "Misses shed because the negative-entry cap was reached", nil, 1)
			return fetchBusy
		}
	}

	created, _ := s.store.InsertPendingFetch(ctx, hash, alg, now+int64((60*time.Second).Seconds()))
	if !created {
		// A row exists: either a crashed pending_fetch (stale-claimable) or an
		// expired negative (backoff-claimable). Re-read and try to claim.
		e, gerr := s.store.Get(ctx, hash)
		if gerr != nil {
			return cache.StatusPendingFetch
		}
		switch e.Status {
		case cache.StatusApproved, cache.StatusPendingReview, cache.StatusRejected:
			return e.Status
		case cache.StatusRejectedFetch:
			if e.ExpiresAt > now {
				return e.Status
			}
			if _, cerr := s.store.ClaimNegativeRefetch(ctx, hash, now+int64(s.backoffFor(e.FailCount+1).Seconds())); cerr != nil {
				return e.Status
			}
			failCount = e.FailCount + 1
		case cache.StatusPendingFetch:
			if e.FetchedAt > now-60 {
				return e.Status // genuinely in flight elsewhere
			}
			claimed, serr := s.store.ClaimStalePendingFetch(ctx, hash, 60, now+60)
			if serr != nil || !claimed {
				return e.Status
			}
			failCount = e.FailCount
		}
	}

	res, ferr := s.fetch.Fetch(ctx, hash, s.cfg.Upstream.FetchSize)
	if ferr != nil {
		reason := "network"
		ttl := s.cfg.Cache.TTLNegative
		if errors.Is(ferr, fetcher.ErrNotFound) {
			reason = "not_found"
			ttl = s.cfg.Cache.TTLNegative404
		} else if failCount > 0 {
			// Repeated network failures back off exponentially up to 24h
			// (SPEC §6.3).
			ttl = s.backoffFor(failCount)
		}
		s.reg.Counter("avater_upstream_requests_total", "Upstream fetches by outcome", map[string]string{"outcome": reason}, 1)
		if merr := s.store.MarkNegative(ctx, hash, alg, reason, now+int64(ttl.Seconds())); merr != nil {
			s.log.Error("persist negative entry", "hash", hash, "err", merr)
		}
		return cache.StatusRejectedFetch
	}
	s.reg.Counter("avater_upstream_requests_total", "Upstream fetches by outcome", map[string]string{"outcome": "ok"}, 1)

	vr, verr := validate.Validate(res.Body, validate.Options{
		MaxDimension: s.cfg.Validate.MaxDimension,
		MaxPixels:    s.cfg.Validate.MaxPixels,
		Reencode:     s.cfg.Validate.Reencode,
		JPEGQuality:  s.cfg.Validate.JPEGQuality,
	})
	if verr != nil {
		reason := validate.ReasonDecodeFail
		if errors.Is(verr, validate.ErrBadMagic) {
			reason = validate.ReasonBadMagic
		} else if errors.Is(verr, validate.ErrTooLarge) {
			reason = validate.ReasonTooLarge
		}
		s.reg.Counter("avater_upstream_requests_total", "Upstream fetches by outcome", map[string]string{"outcome": reason}, 1)
		if merr := s.store.MarkNegative(ctx, hash, alg, reason, now+int64(s.cfg.Cache.TTLNegative.Seconds())); merr != nil {
			s.log.Error("persist negative entry", "hash", hash, "err", merr)
		}
		return cache.StatusRejectedFetch
	}

	ext, eerr := cache.BlobExt(vr.ContentType)
	if eerr != nil {
		s.log.Error("normalized content type unsupported", "hash", hash, "ct", vr.ContentType)
		if merr := s.store.MarkNegative(ctx, hash, alg, validate.ReasonDecodeFail, now+int64(s.cfg.Cache.TTLNegative.Seconds())); merr != nil {
			s.log.Error("persist negative entry", "hash", hash, "err", merr)
		}
		return cache.StatusRejectedFetch
	}
	rel, werr := s.blobs.Write(hash, ext, vr.Data)
	if werr != nil {
		s.log.Error("persist blob", "hash", hash, "err", werr)
		if merr := s.store.MarkNegative(ctx, hash, alg, "blob_write_failed", now+int64(s.cfg.Cache.TTLNegative.Seconds())); merr != nil {
			s.log.Error("persist negative entry", "hash", hash, "err", merr)
		}
		return cache.StatusRejectedFetch
	}

	entry := &cache.Entry{
		Hash:        hash,
		HashAlg:     alg,
		ContentType: vr.ContentType,
		BlobPath:    rel,
		Width:       vr.Width,
		Height:      vr.Height,
		Bytes:       int64(len(vr.Data)),
	}
	if perr := s.store.InsertPendingReview(ctx, entry, s.cfg.Cache.PendingStale); perr != nil {
		s.log.Error("persist pending_review", "hash", hash, "err", perr)
		return cache.StatusRejectedFetch
	}
	s.queue.Enqueue(hash)
	return cache.StatusPendingReview
}

// backoffFor doubles with the failure count, capped at 24h (SPEC §6.3).
func (s *Server) backoffFor(failCount int) time.Duration {
	d := s.cfg.Cache.TTLNegative
	for i := 1; i < failCount && d < 24*time.Hour; i++ {
		d *= 2
	}
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	return d
}

// RetryFetch lets the cleaner proactively refresh expired network-error
// negatives (SPEC §6.3) through the regular miss pipeline.
func (s *Server) RetryFetch(ctx context.Context, hash string) {
	s.fetchAndStore(ctx, hash)
}

// --- serving ---

// serveApproved streams the cached image, scaling to the requested size when
// needed, with strong ETag/304 support (SPEC §4.3, §16.2). Approved images
// always serve their stored (normalized) format.
func (s *Server) serveApproved(w http.ResponseWriter, r *http.Request, req *parsedRequest, e *cache.Entry) {
	etag := fmt.Sprintf(`"%s-%d-%d"`, e.Hash, req.size, e.Rev)
	h := w.Header()
	h.Set("ETag", etag)
	s.policy.ApplyCacheControl(h, cdn.KindApproved)
	s.policy.ApplyTagHeader(h, e.Hash)
	if inm := r.Header.Get("If-None-Match"); etagMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// No scaling needed when the stored image already fits.
	if e.Width <= req.size || e.Width == 0 {
		f, _, err := s.blobs.Open(e.BlobPath)
		if err != nil {
			s.log.Error("open blob", "hash", e.Hash, "err", err)
			s.serveDefault(w, r, req, false)
			return
		}
		defer f.Close()
		h.Set("Content-Type", e.ContentType)
		http.ServeContent(w, r, "", time.Time{}, f)
		s.touchAccess(r.Context(), e.Hash)
		return
	}

	key := fmt.Sprintf("%s/%d/%d", e.Hash, req.size, e.Rev)
	ext := "jpg"
	if e.ContentType == "image/png" {
		ext = "png"
	}
	diskKey := s.scaledDisk.key(e.Hash, int64(req.size), e.Rev, ext)

	data, ok := s.scaled.Get(key)
	if !ok {
		// Second cache line: scaled variants persist across restarts, so a
		// repeat (hash, size) never re-decodes the 2048-wide original.
		if data, ok = s.scaledDisk.get(diskKey); !ok {
			raw, err := s.blobs.Read(e.BlobPath)
			if err != nil {
				s.log.Error("read blob", "hash", e.Hash, "err", err)
				s.serveDefault(w, r, req, false)
				return
			}
			data, err = scaleImage(raw, e.ContentType, req.size)
			if err != nil {
				s.log.Error("scale image", "hash", e.Hash, "err", err)
				s.serveDefault(w, r, req, false)
				return
			}
			s.scaledDisk.put(diskKey, data)
		}
		// Only small renders are memoized in memory to bound heap (SPEC §14).
		if req.size <= 256 {
			s.scaled.Put(key, data)
		}
	}
	h.Set("Content-Type", e.ContentType)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	s.touchAccess(r.Context(), e.Hash)
}

// serveDefault renders the deterministic DiceBear avatar (SPEC §8). explicit
// marks f=y / explicit d= requests, which earn the long immutable TTL since
// their content is a pure function of (hash, style, size).
func (s *Server) serveDefault(w http.ResponseWriter, r *http.Request, req *parsedRequest, explicit bool) {
	style := s.avatars.StyleForD(req.d)

	format := "svg"
	if req.formatPNG || wantsPNG(r) {
		format = "png"
	}

	var (
		data []byte
		ct   string
		err  error
	)
	if format == "png" {
		data, err = s.avatars.PNG(style, req.hash, req.size)
		ct = "image/png"
	} else {
		data, err = s.avatars.SVG(style, req.hash, req.size)
		ct = "image/svg+xml"
	}
	if err != nil {
		s.log.Error("render default avatar", "hash", req.hash, "style", style, "err", err)
		s.fail(w, http.StatusInternalServerError, "default avatar unavailable", "error")
		return
	}

	etag := fmt.Sprintf(`"%s-%d-%s"`, req.hash, req.size, defaultAvatarRev)
	h := w.Header()
	h.Set("ETag", etag)
	if explicit {
		s.policy.ApplyCacheControl(h, cdn.KindDefaultExplicit)
	} else {
		// Pending/rejected state: short TTL so approval surfaces fast
		// (SPEC §16.1). Accept-based negotiation also requires Vary.
		s.policy.ApplyCacheControl(h, cdn.KindDefaultPending)
		h.Add("Vary", "Accept")
	}
	s.policy.ApplyTagHeader(h, req.hash)
	if inm := r.Header.Get("If-None-Match"); etagMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", ct)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	s.reg.Counter("avater_requests_total", "Avatar requests by outcome",
		map[string]string{"result": map[bool]string{true: "default_explicit", false: "default"}[explicit]}, 1)
}

func (s *Server) serve404(w http.ResponseWriter) {
	h := w.Header()
	s.policy.ApplyCacheControl(h, cdn.KindNegative404)
	h.Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, "Not Found")
	s.reg.Counter("avater_requests_total", "Avatar requests by outcome", map[string]string{"result": "negative_404"}, 1)
}

// fail writes a plain-text error; never cached (SPEC §16.1).
func (s *Server) fail(w http.ResponseWriter, code int, msg, result string) {
	h := w.Header()
	s.policy.ApplyCacheControl(h, cdn.KindNoStore)
	h.Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg)
	s.reg.Counter("avater_requests_total", "Avatar requests by outcome", map[string]string{"result": result}, 1)
}

func (s *Server) failRetryAfter(w http.ResponseWriter) {
	h := w.Header()
	s.policy.ApplyCacheControl(h, cdn.KindNoStore)
	h.Set("Retry-After", "1")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, "rate limited")
	s.reg.Counter("avater_requests_total", "Avatar requests by outcome", map[string]string{"result": "rate_limited"}, 1)
}

// touchAccess slides the approved TTL forward, throttled in memory.
func (s *Server) touchAccess(ctx context.Context, hash string) {
	const touchInterval = int64(5 * time.Minute / time.Second)
	now := time.Now().Unix()
	s.touchMu.Lock()
	if last, ok := s.touch[hash]; ok && now-last < touchInterval {
		s.touchMu.Unlock()
		return
	}
	if len(s.touch) > 200_000 {
		s.touch = make(map[string]int64, 1024)
	}
	s.touch[hash] = now
	s.touchMu.Unlock()
	s.store.TouchAccess(ctx, hash, s.cfg.Cache.TTLApproved)
}

// scaleImage decodes, downscales to fit size×size and re-encodes, preserving
// the stored format (JPEG stays JPEG; alpha images re-encode to PNG).
func scaleImage(raw []byte, contentType string, size int) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= size && h <= size {
		return raw, nil
	}
	nw, nh := w, h
	if w > h {
		nh = max(1, h*size/w)
		nw = size
	} else {
		nw = max(1, w*size/h)
		nh = size
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)

	var buf bytes.Buffer
	if contentType == "image/png" || hasAlpha(dst) {
		if err := png.Encode(&buf, dst); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// hasAlpha reports whether any pixel in the image is (partially) transparent.
func hasAlpha(img *image.RGBA) bool {
	if img.Opaque() {
		return false
	}
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 0xff {
			return true
		}
	}
	return false
}

// wantsPNG applies conservative Accept sniffing: only clients that
// explicitly list image/png without any wildcard get PNG (SPEC §16.2 — URL
// based negotiation is preferred; the Vary header is emitted by serveDefault).
func wantsPNG(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	if strings.Contains(accept, "image/svg+xml") ||
		strings.Contains(accept, "*/*") ||
		strings.Contains(accept, "image/*") {
		return false
	}
	return strings.Contains(accept, "image/png")
}

func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	for _, part := range strings.Split(ifNoneMatch, ",") {
		cand := strings.TrimSpace(part)
		cand = strings.TrimPrefix(cand, "W/")
		if cand == etag {
			return true
		}
	}
	return false
}

func clientKey(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
