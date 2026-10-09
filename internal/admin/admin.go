// Package admin implements the management API of SPEC §12: a token-guarded
// HTTP surface on its own port for stats, entry triage, re-moderation and
// cache purging.
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/liueic/avatar/internal/cache"
	"github.com/liueic/avatar/internal/config"
	"github.com/liueic/avatar/internal/metrics"
	"github.com/liueic/avatar/internal/moderate"
)

// HealthInfo describes the moderation engine for /admin/health.
type HealthInfo struct {
	Engine        string `json:"engine"`
	ModelVer      string `json:"model_ver"`
	Ready         bool   `json:"ready"`
	LastInference string `json:"last_inference,omitempty"`
	ORTVersion    string `json:"onnxruntime_version,omitempty"`
}

// Deps bundles the admin surface's collaborators.
type Deps struct {
	Cfg    config.Config
	Store  *cache.Store
	Blobs  *cache.Blobs
	Queue  *moderate.Service
	Purger moderate.Purger
	Log    *slog.Logger
	Reg    *metrics.Registry
	Health func() HealthInfo
}

// Server is the admin HTTP handler set.
type Server struct {
	deps Deps
}

// New builds the admin server.
func New(d Deps) *Server { return &Server{deps: d} }

// Handler builds the admin mux wrapped in token authentication.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/stats", s.stats)
	mux.HandleFunc("GET /admin/entries", s.listEntries)
	mux.HandleFunc("POST /admin/entries/{hash}/approve", s.manual(true))
	mux.HandleFunc("POST /admin/entries/{hash}/reject", s.manual(false))
	mux.HandleFunc("POST /admin/entries/{hash}/remoderate", s.remoderateOne)
	mux.HandleFunc("POST /admin/remoderate", s.remoderateBatch)
	mux.HandleFunc("DELETE /admin/entries/{hash}", s.deleteEntry)
	mux.HandleFunc("POST /admin/purge", s.purge)
	mux.HandleFunc("GET /admin/health", s.health)
	return s.auth(s.noStore(mux))
}

func (s *Server) noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// auth enforces the bearer token with a constant-time comparison.
func (s *Server) auth(next http.Handler) http.Handler {
	token := s.deps.Cfg.AdminToken
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		const prefix = "Bearer "
		ok := len(got) > len(prefix) &&
			subtle.ConstantTimeCompare([]byte(got[len(prefix):]), []byte(token)) == 1
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	counts, err := s.deps.Store.CountByStatus(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	bytesByStatus, _ := s.deps.Store.BytesByStatus(ctx)
	out := map[string]any{
		"entries":      map[string]int64{},
		"bytes":        map[string]int64{},
		"queue_depth":  s.deps.Queue.Depth(),
		"engine":       s.deps.Queue.EngineStatus(),
		"cache_max":    s.deps.Cfg.Cache.MaxBytes,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}
	entries := out["entries"].(map[string]int64)
	var total int64
	for _, st := range cache.AllStatuses() {
		entries[string(st)] = counts[st]
		total += counts[st]
	}
	entries["total"] = total
	bmap := out["bytes"].(map[string]int64)
	for st, n := range bytesByStatus {
		bmap[string(st)] = n
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listEntries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	var status *cache.Status
	if raw := q.Get("status"); raw != "" {
		st, err := cache.ParseStatus(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		status = &st
	}
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "limit must be 1..500"})
			return
		}
		limit = n
	}
	offset := 0
	if v := q.Get("cursor"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cursor must be a non-negative offset"})
			return
		}
		offset = n
	}

	entries, err := s.deps.Store.ListEntries(ctx, status, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	type entryView struct {
		Hash           string `json:"hash"`
		HashAlg        string `json:"hash_alg"`
		Status         string `json:"status"`
		ContentType    string `json:"content_type,omitempty"`
		Width          int    `json:"width,omitempty"`
		Height         int    `json:"height,omitempty"`
		Bytes          int64  `json:"bytes,omitempty"`
		Rev            int64  `json:"rev"`
		VerdictScores  string `json:"verdict_scores,omitempty"`
		ModelVer       string `json:"model_ver,omitempty"`
		GrayZone       bool   `json:"gray_zone"`
		ManualOverride bool   `json:"manual_override"`
		FailReason     string `json:"fail_reason,omitempty"`
		FetchedAt      int64  `json:"fetched_at,omitempty"`
		ReviewedAt     int64  `json:"reviewed_at,omitempty"`
		ExpiresAt      int64  `json:"expires_at,omitempty"`
	}
	views := make([]entryView, 0, len(entries))
	for _, e := range entries {
		views = append(views, entryView{
			Hash: e.Hash, HashAlg: string(e.HashAlg), Status: string(e.Status),
			ContentType: e.ContentType, Width: e.Width, Height: e.Height,
			Bytes: e.Bytes, Rev: e.Rev, VerdictScores: e.VerdictScores,
			ModelVer: e.ModelVer, GrayZone: e.GrayZone, ManualOverride: e.ManualOverride,
			FailReason: e.FailReason, FetchedAt: e.FetchedAt,
			ReviewedAt: e.ReviewedAt, ExpiresAt: e.ExpiresAt,
		})
	}
	next := ""
	if len(entries) == limit {
		next = strconv.Itoa(offset + limit)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": views, "next_cursor": next})
}

func (s *Server) manual(approved bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		if !cache.ValidateHash(hash) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid hash"})
			return
		}
		force := r.URL.Query().Get("force") == "true"
		err := s.deps.Store.ManualSetStatus(r.Context(), hash, approved, force,
			s.deps.Cfg.Cache.TTLApproved, s.deps.Cfg.Cache.TTLRejected)
		switch {
		case errors.Is(err, cache.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "entry not found"})
			return
		case err != nil:
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		// Human decision changes what the URL must serve: withdraw stale CDN
		// copies immediately (SPEC §16.3).
		s.deps.Purger.PurgeTagsAsync(hash)
		s.deps.Log.Info("admin manual decision", "hash", hash, "status", verdict(approved), "force", force)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hash": hash, "status": verdict(approved)})
	}
}

func (s *Server) remoderateOne(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !cache.ValidateHash(hash) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid hash"})
		return
	}
	force := r.URL.Query().Get("force") == "true"
	var (
		requeued []string
		err      error
	)
	if force {
		requeued, err = s.deps.Store.ForceRequeue(r.Context(), []string{hash})
	} else {
		requeued, err = s.deps.Store.RequeueForReview(r.Context(), []string{hash}, true)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if len(requeued) == 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "entry not eligible (missing, or protected by a manual override without force=true)",
		})
		return
	}
	s.deps.Queue.EnqueueBatch(requeued)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hash": hash, "queued": len(requeued)})
}

func (s *Server) remoderateBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	var status cache.Status
	if raw := q.Get("status"); raw != "" {
		st, err := cache.ParseStatus(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		status = st
	}
	const batch = 500
	requeued, err := s.deps.Store.RequeueByFilter(ctx, q.Get("model_ver"), status, batch, false)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.deps.Queue.EnqueueBatch(requeued)
	s.deps.Purger.PurgeTagsAsync(requeued...)
	s.deps.Log.Info("admin batch remoderate", "count", len(requeued), "model_ver", q.Get("model_ver"), "status", q.Get("status"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "queued": len(requeued)})
}

func (s *Server) deleteEntry(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !cache.ValidateHash(hash) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid hash"})
		return
	}
	blobPath, err := s.deps.Store.DeleteReturning(r.Context(), hash)
	if errors.Is(err, cache.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "entry not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if derr := s.deps.Blobs.Delete(blobPath); derr != nil {
		s.deps.Log.Error("admin delete blob", "hash", hash, "err", derr)
	}
	s.deps.Purger.PurgeTagsAsync(hash)
	s.deps.Log.Info("admin deleted entry", "hash", hash)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hash": hash})
}

// purge runs cache maintenance: status=expired removes expired entries (and
// their blobs); status=all wipes every entry and blob (destructive).
func (s *Server) purge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.URL.Query().Get("status") {
	case "expired":
		now := time.Now().Unix()
		removed, err := s.deps.Store.DeleteExpired(ctx, now,
			cache.StatusApproved, cache.StatusRejectedFetch, cache.StatusPendingFetch)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		kept, err := s.deps.Store.ExpireKeepRecord(ctx, now)
		if err != nil {
			s.deps.Log.Error("purge keep-record pass", "err", err)
		}
		for _, e := range removed {
			_ = s.deps.Blobs.Delete(e.BlobPath)
		}
		for _, e := range kept {
			_ = s.deps.Blobs.Delete(e.BlobPath)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": len(removed), "blob_only": len(kept)})
	case "all":
		n, err := s.deps.Store.DeleteAll(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if err := s.deps.Blobs.DeleteAll(); err != nil {
			s.deps.Log.Error("purge all blobs", "err", err)
		}
		s.deps.Log.Warn("admin purged entire cache", "entries", n)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": n})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "status must be expired or all"})
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	info := HealthInfo{Engine: "unknown"}
	if s.deps.Health != nil {
		info = s.deps.Health()
	}
	code := http.StatusOK
	if !info.Ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, info)
}

func verdict(approved bool) string {
	if approved {
		return "approved"
	}
	return "rejected"
}
