// Package cdn implements the cache-header policy of SPEC §16.1 and the
// content-withdrawal purge channel of SPEC §16.3 (Cloudflare tag purge /
// Fastly surrogate-key purge).
package cdn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/liueic/avater/internal/config"
	"github.com/liueic/avater/internal/metrics"
)

// Kind classifies a response for cache-policy purposes (SPEC §16.1).
type Kind int

const (
	// KindApproved is a long-cached real avatar.
	KindApproved Kind = iota
	// KindDefaultExplicit is a deterministic default requested explicitly
	// via f=y or d= — content never changes for a given (hash, style, size).
	KindDefaultExplicit
	// KindDefaultPending is a default avatar served because the entry is
	// pending/rejected — short TTL so approval surfaces quickly.
	KindDefaultPending
	// KindNegative404 is the negative cache for "upstream has no avatar".
	KindNegative404
	// KindNoStore marks errors and infrastructure endpoints.
	KindNoStore
)

// Policy renders cache headers for responses.
type Policy struct {
	cfg config.CDNConfig
}

// NewPolicy builds the header policy from config.
func NewPolicy(cfg config.CDNConfig) *Policy {
	return &Policy{cfg: cfg}
}

// ApplyCacheControl sets the Cache-Control header for the response kind.
func (p *Policy) ApplyCacheControl(w http.Header, kind Kind) {
	switch kind {
	case KindApproved:
		var b strings.Builder
		fmt.Fprintf(&b, "public, max-age=%d, s-maxage=%d",
			int(p.cfg.TTLApproved.Seconds()), int(p.cfg.TTLApprovedCDN.Seconds()))
		if p.cfg.SWR > 0 {
			fmt.Fprintf(&b, ", stale-while-revalidate=%d", int(p.cfg.SWR.Seconds()))
		}
		if p.cfg.SIE > 0 {
			fmt.Fprintf(&b, ", stale-if-error=%d", int(p.cfg.SIE.Seconds()))
		}
		w.Set("Cache-Control", b.String())
	case KindDefaultExplicit:
		// Content is fully determined by (hash, style, size, format) and can
		// never change — safe to mark immutable (SPEC §16.1).
		w.Set("Cache-Control", "public, max-age=2592000, immutable")
	case KindDefaultPending:
		var b strings.Builder
		fmt.Fprintf(&b, "public, max-age=%d", int(p.cfg.TTLPending.Seconds()))
		if p.cfg.TTLPending > 0 {
			fmt.Fprintf(&b, ", s-maxage=%d", int((5 * p.cfg.TTLPending).Seconds()))
		}
		w.Set("Cache-Control", b.String())
	case KindNegative404:
		w.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(p.cfg.TTLNegative.Seconds())))
	case KindNoStore:
		w.Set("Cache-Control", "no-store")
	}
}

// ApplyTagHeader emits the CDN cache tag for a hash: Cloudflare's Cache-Tag
// or Fastly's Surrogate-Key, per configuration (SPEC §16.3).
func (p *Policy) ApplyTagHeader(w http.Header, hash string) {
	if !p.cfg.CacheTag {
		return
	}
	switch p.cfg.Provider {
	case "cloudflare":
		w.Add("Cache-Tag", TagFor(hash))
	case "fastly":
		w.Add("Surrogate-Key", TagFor(hash))
	default:
		// Emit Cloudflare semantics even without a provider configured so
		// operators can inspect tags at the origin.
		w.Add("Cache-Tag", TagFor(hash))
	}
}

// TagFor is the cache tag of a hash.
func TagFor(hash string) string { return "av-" + hash }

// Purger withdraws CDN-cached content after moderation changes. Purge calls
// happen off the request path; failures retry with backoff and are logged
// (SPEC §16.3: never block the management API on purge).
type Purger struct {
	provider  string
	token     string
	zoneID    string
	serviceID string
	client    *http.Client
	log       *slog.Logger
	reg       *metrics.Registry

	mu   sync.Mutex
	pend map[string]struct{}
	ch   chan string
	done chan struct{}
	once sync.Once
}

// NewPurger builds a purger; provider "none"/"" disables network calls.
func NewPurger(cfg config.CDNConfig, log *slog.Logger, reg *metrics.Registry) *Purger {
	p := &Purger{
		provider:  cfg.Provider,
		token:     cfg.PurgeToken,
		zoneID:    cfg.ZoneID,
		serviceID: cfg.FastlyServiceID,
		client:    &http.Client{Timeout: 10 * time.Second},
		log:       log,
		reg:       reg,
		pend:      make(map[string]struct{}),
		ch:        make(chan string, 1024),
		done:      make(chan struct{}),
	}
	if p.enabled() {
		go p.loop()
	}
	return p
}

func (p *Purger) enabled() bool {
	switch p.provider {
	case "cloudflare":
		return p.token != "" && p.zoneID != ""
	case "fastly":
		return p.token != "" && p.serviceID != ""
	}
	return false
}

// PurgeTagsAsync schedules a tag purge; it never blocks and never errors the
// caller.
func (p *Purger) PurgeTagsAsync(hashes ...string) {
	if p == nil || !p.enabled() || len(hashes) == 0 {
		return
	}
	p.mu.Lock()
	for _, h := range hashes {
		if _, dup := p.pend[h]; dup {
			continue
		}
		p.pend[h] = struct{}{}
		select {
		case p.ch <- h:
		default:
			// Channel full: keep the dedup entry, drop the notification — the
			// next change to this hash will retry.
			delete(p.pend, h)
			p.reg.Counter("avater_purge_dropped_total", "CDN purge notifications dropped (queue full)", nil, 1)
		}
	}
	p.mu.Unlock()
}

// Stop terminates the purge worker.
func (p *Purger) Stop() {
	if p.enabled() {
		p.once.Do(func() { close(p.done) })
	}
}

func (p *Purger) loop() {
	const batchWait = 2 * time.Second
	batch := make([]string, 0, 32)
	ticker := time.NewTicker(batchWait)
	defer ticker.Stop()
	for {
		select {
		case h := <-p.ch:
			p.mu.Lock()
			delete(p.pend, h)
			p.mu.Unlock()
			batch = append(batch, h)
			// Small coalescing window so a burst of reviews costs one call.
			if len(batch) >= 30 {
				p.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				p.flush(batch)
				batch = batch[:0]
			}
		case <-p.done:
			return
		}
	}
}

func (p *Purger) flush(hashes []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var err error
	switch p.provider {
	case "cloudflare":
		err = p.purgeCloudflare(ctx, hashes)
	case "fastly":
		err = p.purgeFastly(ctx, hashes)
	}
	if err != nil {
		p.reg.Counter("avater_purge_failures_total", "CDN purge calls that failed", nil, 1)
		p.log.Error("cdn purge failed", "provider", p.provider, "hashes", len(hashes), "err", err)
		return
	}
	p.reg.Counter("avater_purges_total", "CDN purge calls issued", map[string]string{"provider": p.provider}, 1)
}

func (p *Purger) purgeCloudflare(ctx context.Context, hashes []string) error {
	// Cloudflare purge-by-tag (Enterprise): up to 30 tags per request.
	tags := make([]string, len(hashes))
	for i, h := range hashes {
		tags[i] = TagFor(h)
	}
	body, _ := json.Marshal(map[string][]string{"tags": tags})
	url := "https://api.cloudflare.com/client/v4/zones/" + p.zoneID + "/purge_cache"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	return p.do(req)
}

func (p *Purger) purgeFastly(ctx context.Context, hashes []string) error {
	// Fastly surrogate-key purge: keys space-separated in the header.
	keys := make([]string, len(hashes))
	for i, h := range hashes {
		keys[i] = TagFor(h)
	}
	url := "https://api.fastly.com/service/" + p.serviceID + "/purge"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Fastly-Key", p.token)
	req.Header.Set("Surrogate-Key", strings.Join(keys, " "))
	return p.do(req)
}

func (p *Purger) do(req *http.Request) error {
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("purge status %d", resp.StatusCode)
	}
	return nil
}
