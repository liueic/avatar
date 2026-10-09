// Package fetcher pulls avatar images from the Gravatar whitelist upstreams
// with layered SSRF protection (SPEC §6).
package fetcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sentinel fetch outcomes.
var (
	ErrNotFound    = errors.New("upstream has no avatar for this hash") // HTTP 404
	ErrBlocked     = errors.New("target blocked by SSRF policy")
	ErrTooLarge    = errors.New("upstream response exceeds size limit")
	ErrStatus      = errors.New("unexpected upstream status")
	ErrRateLimited = errors.New("upstream rate limit exhausted")
	ErrRedirect    = errors.New("upstream redirect not allowed")
)

// Options tunes the fetcher. AllowPrivateIPs and HTTPClient exist exclusively
// for tests against loopback httptest servers; production wiring must leave
// them zero.
type Options struct {
	AllowPrivateIPs bool
	// UserAgent overrides the default fetcher UA.
	UserAgent string
	// HTTPClient replaces the SSRF-hardened client wholesale (tests only:
	// e.g. httptest.NewTLSServer's client trusts the test certificate).
	HTTPClient *http.Client
}

const defaultUA = "avater/1.0 (+https://github.com/liueic/avater)"

// Fetcher performs whitelist-constrained upstream fetches.
type Fetcher struct {
	client   *http.Client
	dialer   *net.Dialer
	hosts    map[string]struct{}
	reqHost  string
	maxBytes int64
	timeout  time.Duration
	ua       string

	allowAll bool // test-only: permit private IPs (loopback httptest upstreams)
}

// New builds a fetcher. cfg fields: allowed hosts, timeouts, size cap.
type Config struct {
	AllowedHosts []string
	MaxBytes     int64
	Timeout      time.Duration
	DialTimeout  time.Duration
	TLSTimeout   time.Duration
}

// TokenBucket is a minimal refill-based rate limiter used for upstream pacing.
type TokenBucket struct {
	mu       chan struct{}
	interval time.Duration
	stop     chan struct{}
	stopOnce sync.Once
}

// NewTokenBucket creates a bucket admitting one event every 1/rps interval,
// with an initial burst capacity. Call Stop to release the refill goroutine.
func NewTokenBucket(rps float64, burst int) *TokenBucket {
	if rps <= 0 {
		rps = 10
	}
	if burst < 1 {
		burst = 1
	}
	tb := &TokenBucket{
		mu:       make(chan struct{}, burst),
		interval: time.Duration(float64(time.Second) / rps),
		stop:     make(chan struct{}),
	}
	for i := 0; i < burst; i++ {
		tb.mu <- struct{}{}
	}
	go tb.refill()
	return tb
}

// Stop terminates the refill goroutine.
func (tb *TokenBucket) Stop() { tb.stopOnce.Do(func() { close(tb.stop) }) }

func (tb *TokenBucket) refill() {
	t := time.NewTicker(tb.interval)
	defer t.Stop()
	for {
		select {
		case <-tb.stop:
			return
		case <-t.C:
			select {
			case tb.mu <- struct{}{}:
			default:
			}
		}
	}
}

// Acquire blocks until a token is available or ctx is done.
func (tb *TokenBucket) Acquire(ctx context.Context) error {
	select {
	case <-tb.mu:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// New constructs a Fetcher from config and options.
func New(cfg Config, opts Options) (*Fetcher, error) {
	if len(cfg.AllowedHosts) == 0 {
		return nil, fmt.Errorf("fetcher: empty host whitelist")
	}
	if cfg.MaxBytes <= 0 {
		return nil, fmt.Errorf("fetcher: max bytes must be positive")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 3 * time.Second
	}
	if cfg.TLSTimeout <= 0 {
		cfg.TLSTimeout = 3 * time.Second
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUA
	}

	hosts := make(map[string]struct{}, len(cfg.AllowedHosts))
	for _, h := range cfg.AllowedHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			return nil, fmt.Errorf("fetcher: empty entry in host whitelist")
		}
		hosts[h] = struct{}{}
	}
	reqHost := strings.ToLower(strings.TrimSpace(cfg.AllowedHosts[0]))

	dialer := &net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 30 * time.Second}
	f := &Fetcher{
		dialer:   dialer,
		hosts:    hosts,
		reqHost:  reqHost,
		maxBytes: cfg.MaxBytes,
		timeout:  cfg.Timeout,
		ua:       ua,
		allowAll: opts.AllowPrivateIPs,
	}
	if opts.HTTPClient != nil {
		f.client = opts.HTTPClient
		return f, nil
	}
	f.client = &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil, // never route upstream fetches through a proxy
			DialContext:           f.dialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          20,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   cfg.TLSTimeout,
			ResponseHeaderTimeout: cfg.Timeout,
			ExpectContinueTimeout: time.Second,
		},
		// SPEC §6.2.3: redirects are forbidden; the http client surfaces the
		// first response instead of following.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return f, nil
}

// dialContext resolves addr, validates every resolved IP against the SSRF
// policy, and connects to a validated IP directly (pinning, SPEC §6.2.7).
func (f *Fetcher) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: bad dial address: %v", ErrBlocked, err)
	}
	// Defense in depth: upstream URLs are always https on 443. (Test mode
	// with allowAll accepts arbitrary loopback ports.)
	if port, perr := strconv.ParseUint(portStr, 10, 16); perr != nil || (port != 443 && !f.allowAll) {
		return nil, fmt.Errorf("%w: port %q not allowed", ErrBlocked, portStr)
	}

	// Whitelist check at dial time too: even if a hostile URL ever reaches
	// the client, the connection is refused (SPEC §6.2.1).
	if !f.allowAll && !f.CheckHost(host) {
		return nil, fmt.Errorf("%w: host %s is not whitelisted", ErrBlocked, host)
	}

	var ips []netip.Addr
	if a, perr := netip.ParseAddr(host); perr == nil {
		ips = append(ips, a.Unmap())
	} else {
		resolver := f.dialer.Resolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		resolved, rerr := resolver.LookupHost(ctx, host)
		if rerr != nil {
			return nil, fmt.Errorf("fetcher: resolve %s: %w", host, rerr)
		}
		for _, r := range resolved {
			a, perr := netip.ParseAddr(r)
			if perr != nil {
				continue
			}
			ips = append(ips, a.Unmap())
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: no usable addresses for %s", ErrBlocked, host)
	}

	var lastErr error
	for _, ip := range ips {
		if err := CheckIP(ip); err != nil && !f.allowAll {
			lastErr = fmt.Errorf("%w: %s: %v", ErrBlocked, ip, err)
			continue
		}
		conn, derr := f.dialer.DialContext(ctx, network, netip.AddrPortFrom(ip, 443).String())
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: all resolved IPs rejected for %s", ErrBlocked, host)
	}
	return nil, lastErr
}

// FetchResult is a successful upstream fetch.
type FetchResult struct {
	Body        []byte
	ContentType string // as reported by upstream; only informational
}

// Fetch pulls the avatar for hash. The returned error wraps ErrNotFound when
// the upstream reports no avatar (HTTP 404), enabling negative caching.
func (f *Fetcher) Fetch(ctx context.Context, hash string, size int, tb *TokenBucket) (*FetchResult, error) {
	if tb != nil {
		if err := tb.Acquire(ctx); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRateLimited, err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	url := "https://" + f.reqHost + "/avatar/" + hash + "?s=" + strconv.Itoa(size) + "&d=404"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetcher: build request: %w", err)
	}
	req.Header.Set("User-Agent", f.ua)
	req.Header.Set("Accept", "image/jpeg, image/png, image/gif, image/webp")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetcher: upstream request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through to body handling
	case http.StatusNotFound:
		return nil, ErrNotFound
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return nil, fmt.Errorf("%w: got %d", ErrRedirect, resp.StatusCode)
	default:
		return nil, fmt.Errorf("%w: %d", ErrStatus, resp.StatusCode)
	}

	if resp.ContentLength > f.maxBytes {
		return nil, ErrTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetcher: read body: %w", err)
	}
	if int64(len(body)) > f.maxBytes {
		return nil, ErrTooLarge
	}
	return &FetchResult{Body: body, ContentType: resp.Header.Get("Content-Type")}, nil
}

// CheckHost reports whether host is on the whitelist (exact match, no
// wildcard or suffix tricks — SPEC §6.2.1).
func (f *Fetcher) CheckHost(host string) bool {
	_, ok := f.hosts[strings.ToLower(strings.TrimSuffix(host, "."))]
	return ok
}
