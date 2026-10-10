package server

import (
	_ "embed"
	"net/http"

	"github.com/liueic/avatar/internal/cdn"
)

//go:embed landing.html
var landingHTML []byte

// handleLanding serves the static product landing page at GET /. The page is
// fully self-contained (inline CSS, same-origin avatar images), so the global
// "default-src 'none'" CSP is relaxed just enough for it to render.
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; base-uri 'none'; form-action 'none'")
	h.Set("Content-Type", "text/html; charset=utf-8")
	s.policy.ApplyCacheControl(h, cdn.KindApproved)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(landingHTML)
}
