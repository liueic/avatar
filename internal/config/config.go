// Package config loads and validates avater configuration.
//
// Precedence: environment variables override the TOML file (AVATER_CONFIG).
// Every key maps to an env var named AVATER_<SECTION>_<FIELD> (see env.go).
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the full service configuration. Field defaults follow SPEC §13.
type Config struct {
	Listen      string `toml:"listen"`
	AdminListen string `toml:"admin_listen"`
	AdminToken  string `toml:"admin_token"`

	Log          LogConfig           `toml:"log"`
	RateLimit    RateLimitConfig     `toml:"rate_limit"`
	Upstream     UpstreamConfig      `toml:"upstream"`
	Validate     ValidateConfig      `toml:"validate"`
	Moderation   ModerationConfig    `toml:"moderation"`
	Cache        CacheConfig         `toml:"cache"`
	CDN          CDNConfig           `toml:"cdn"`
	DefaultAvatr DefaultAvatarConfig `toml:"default_avatar"`
	Metrics      MetricsConfig       `toml:"metrics"`
}

type LogConfig struct {
	Format      string `toml:"format"`        // json | text
	Level       string `toml:"level"`         // debug | info | warn | error
	AccessLog   bool   `toml:"access_log"`    // per-request access log
	ClientIPLog string `toml:"client_ip_log"` // none | trunc24 | full
}

type RateLimitConfig struct {
	Enabled     bool    `toml:"enabled"`
	GlobalRPS   float64 `toml:"global_rps"`
	GlobalBurst int     `toml:"global_burst"`
	PerIPRPS    float64 `toml:"per_ip_rps"`
	PerIPBurst  int     `toml:"per_ip_burst"`
}

type UpstreamConfig struct {
	AllowedHosts   []string      `toml:"allowed_hosts"`
	MaxBytes       int64         `toml:"max_bytes"`
	Timeout        time.Duration `toml:"timeout"`
	DialTimeout    time.Duration `toml:"dial_timeout"`
	TLSTimeout     time.Duration `toml:"tls_timeout"`
	RateLimitRPS   float64       `toml:"rate_limit_rps"`
	RateLimitBurst int           `toml:"rate_limit_burst"`
	FetchSize      int           `toml:"fetch_size"` // s= param used when pulling from upstream
}

type ValidateConfig struct {
	MaxDimension int   `toml:"max_dimension"`
	MaxPixels    int64 `toml:"max_pixels"`
	Reencode     bool  `toml:"reencode"`
	JPEGQuality  int   `toml:"jpeg_quality"`
}

type ModerationConfig struct {
	Engine                 string        `toml:"engine"`      // onnx | none
	NonePolicy             string        `toml:"none_policy"` // approve | reject (engine=none only)
	ModelPath              string        `toml:"model_path"`
	ModelSHA256            string        `toml:"model_sha256"`
	ORTLibPath             string        `toml:"ort_lib_path"`
	Workers                int           `toml:"workers"`
	QueueSize              int           `toml:"queue_size"`
	MinInterval            time.Duration `toml:"min_interval"`
	InferenceTimeout       time.Duration `toml:"inference_timeout"`
	MaxAttempts            int           `toml:"max_attempts"`
	ThresholdNSFW          float64       `toml:"threshold_nsfw"`
	ThresholdNSFL          float64       `toml:"threshold_nsfl"`
	GrayZoneAction         string        `toml:"gray_zone_action"` // reject | approve
	GrayZoneThreshold      float64       `toml:"gray_zone_threshold"`
	ReReviewOnModelUpgrade bool          `toml:"re_review_on_model_upgrade"`
}

type CacheConfig struct {
	Dir            string        `toml:"dir"`
	MaxBytes       int64         `toml:"max_bytes"`
	TTLApproved    time.Duration `toml:"ttl_approved"`
	TTLNegative    time.Duration `toml:"ttl_negative"`     // network-error negative entries
	TTLNegative404 time.Duration `toml:"ttl_negative_404"` // upstream says "no such avatar"
	TTLRejected    time.Duration `toml:"ttl_rejected"`     // rejected blob retention
	PendingStale   time.Duration `toml:"pending_stale"`    // re-enqueue pending_review older than this
	CleanInterval  time.Duration `toml:"clean_interval"`
	DefaultLRU     int           `toml:"default_lru"` // in-memory default-avatar cache entries
}

type CDNConfig struct {
	Provider        string        `toml:"provider"` // none | cloudflare | fastly
	CacheTag        bool          `toml:"cache_tag"`
	PurgeToken      string        `toml:"purge_token"`
	ZoneID          string        `toml:"zone_id"`           // cloudflare
	FastlyServiceID string        `toml:"fastly_service_id"` // fastly
	TTLPending      time.Duration `toml:"ttl_pending"`
	TTLApproved     time.Duration `toml:"ttl_approved"`
	TTLApprovedCDN  time.Duration `toml:"ttl_approved_cdn"`
	TTLNegative     time.Duration `toml:"ttl_negative"`
	SWR             time.Duration `toml:"swr"` // stale-while-revalidate for approved
	SIE             time.Duration `toml:"sie"` // stale-if-error for approved
}

type DefaultAvatarConfig struct {
	Style      string `toml:"style"`       // identicon (default), pixel-art
	RetroStyle string `toml:"retro_style"` // style used for d=retro
}

type MetricsConfig struct {
	Enabled bool `toml:"enabled"`
}

// Default returns a Config populated with the SPEC §13 defaults.
func Default() Config {
	var c Config
	c.Listen = ":8080"
	c.AdminListen = ":8081"
	c.AdminToken = ""

	c.Log.Format = "json"
	c.Log.Level = "info"
	c.Log.AccessLog = false
	c.Log.ClientIPLog = "none"

	c.RateLimit.Enabled = true
	c.RateLimit.GlobalRPS = 200
	c.RateLimit.GlobalBurst = 400
	c.RateLimit.PerIPRPS = 20
	c.RateLimit.PerIPBurst = 40

	c.Upstream.AllowedHosts = []string{
		"secure.gravatar.com",
		"www.gravatar.com",
		"0.gravatar.com",
		"1.gravatar.com",
		"2.gravatar.com",
	}
	c.Upstream.MaxBytes = 10 << 20 // 10 MB
	c.Upstream.Timeout = 5 * time.Second
	c.Upstream.DialTimeout = 3 * time.Second
	c.Upstream.TLSTimeout = 3 * time.Second
	c.Upstream.RateLimitRPS = 10
	c.Upstream.RateLimitBurst = 20
	c.Upstream.FetchSize = 2048

	c.Validate.MaxDimension = 2048
	c.Validate.MaxPixels = 2048 * 2048
	c.Validate.Reencode = true
	c.Validate.JPEGQuality = 85

	c.Moderation.Engine = "onnx"
	c.Moderation.NonePolicy = "reject"
	c.Moderation.ModelPath = "models/image-safety-classifier-xs.onnx"
	c.Moderation.ModelSHA256 = ""
	c.Moderation.ORTLibPath = "libonnxruntime.so"
	c.Moderation.Workers = 1
	c.Moderation.QueueSize = 256
	c.Moderation.MinInterval = 200 * time.Millisecond
	c.Moderation.InferenceTimeout = 10 * time.Second
	c.Moderation.MaxAttempts = 3
	c.Moderation.ThresholdNSFW = 0.5
	c.Moderation.ThresholdNSFL = 0.5
	c.Moderation.GrayZoneAction = "reject"
	c.Moderation.GrayZoneThreshold = 0.6
	c.Moderation.ReReviewOnModelUpgrade = true

	c.Cache.Dir = "data"
	c.Cache.MaxBytes = 1 << 30 // 1 GB
	c.Cache.TTLApproved = 30 * 24 * time.Hour
	c.Cache.TTLNegative = 6 * time.Hour
	c.Cache.TTLNegative404 = 24 * time.Hour
	c.Cache.TTLRejected = 90 * 24 * time.Hour
	c.Cache.PendingStale = 24 * time.Hour
	c.Cache.CleanInterval = 10 * time.Minute
	c.Cache.DefaultLRU = 1024

	c.CDN.Provider = "none"
	c.CDN.CacheTag = true
	c.CDN.TTLPending = 60 * time.Second
	c.CDN.TTLApproved = 24 * time.Hour
	c.CDN.TTLApprovedCDN = 7 * 24 * time.Hour
	c.CDN.TTLNegative = 5 * time.Minute
	c.CDN.SWR = 24 * time.Hour
	c.CDN.SIE = 7 * 24 * time.Hour

	c.DefaultAvatr.Style = "thumbs"   // cute DiceBear style (CC0)
	c.DefaultAvatr.RetroStyle = "lorelei"

	c.Metrics.Enabled = false
	return c
}

// Load reads the TOML file named by AVATER_CONFIG (or the given path override),
// then overlays environment variables. Callers must run Validate afterwards.
func Load(pathOverride string) (Config, error) {
	c := Default()

	path := pathOverride
	if path == "" {
		path = os.Getenv("AVATER_CONFIG")
	}
	if path != "" {
		if _, err := toml.DecodeFile(path, &c); err != nil {
			return c, fmt.Errorf("config: decode %s: %w", path, err)
		}
	}

	if err := c.applyEnv(); err != nil {
		return c, err
	}
	if err := c.Check(); err != nil {
		return c, err
	}
	return c, nil
}

// Check verifies semantic constraints that defaults already satisfy.
func (c *Config) Check() error {
	if c.Listen == "" {
		return fmt.Errorf("config: listen must not be empty")
	}
	if c.AdminListen != "" && c.AdminToken == "" {
		return fmt.Errorf("config: admin_token must be set when admin_listen is enabled")
	}
	if c.Upstream.MaxBytes <= 0 {
		return fmt.Errorf("config: upstream.max_bytes must be positive")
	}
	if c.Upstream.Timeout <= 0 {
		return fmt.Errorf("config: upstream.timeout must be positive")
	}
	if len(c.Upstream.AllowedHosts) == 0 {
		return fmt.Errorf("config: upstream.allowed_hosts must not be empty")
	}
	for _, h := range c.Upstream.AllowedHosts {
		if h == "" {
			return fmt.Errorf("config: upstream.allowed_hosts contains an empty host")
		}
	}
	if c.Validate.MaxDimension < 16 {
		return fmt.Errorf("config: validate.max_dimension must be >= 16")
	}
	if c.Validate.MaxPixels <= 0 {
		return fmt.Errorf("config: validate.max_pixels must be positive")
	}
	switch c.Moderation.Engine {
	case "onnx":
		if c.Moderation.ModelPath == "" {
			return fmt.Errorf("config: moderation.model_path required for engine=onnx")
		}
	case "none":
		if c.Moderation.NonePolicy != "approve" && c.Moderation.NonePolicy != "reject" {
			return fmt.Errorf("config: moderation.none_policy must be approve or reject")
		}
	default:
		return fmt.Errorf("config: moderation.engine must be onnx or none")
	}
	if c.Moderation.Workers < 1 || c.Moderation.Workers > 2 {
		return fmt.Errorf("config: moderation.workers must be 1 or 2")
	}
	if c.Moderation.QueueSize < 1 {
		return fmt.Errorf("config: moderation.queue_size must be positive")
	}
	if c.Moderation.GrayZoneAction != "reject" && c.Moderation.GrayZoneAction != "approve" {
		return fmt.Errorf("config: moderation.gray_zone_action must be reject or approve")
	}
	if c.Moderation.InferenceTimeout <= 0 {
		c.Moderation.InferenceTimeout = 10 * time.Second
	}
	if c.Moderation.MaxAttempts < 1 {
		c.Moderation.MaxAttempts = 3
	}
	if c.Cache.Dir == "" {
		return fmt.Errorf("config: cache.dir must not be empty")
	}
	if c.Cache.MaxBytes <= 0 {
		return fmt.Errorf("config: cache.max_bytes must be positive")
	}
	if c.Cache.TTLApproved <= 0 || c.Cache.TTLNegative <= 0 || c.Cache.TTLNegative404 <= 0 {
		return fmt.Errorf("config: cache TTLs must be positive")
	}
	switch c.CDN.Provider {
	case "", "none", "cloudflare", "fastly":
	default:
		return fmt.Errorf("config: cdn.provider must be none, cloudflare or fastly")
	}
	if c.CDN.Provider == "cloudflare" && (c.CDN.PurgeToken == "" || c.CDN.ZoneID == "") {
		return fmt.Errorf("config: cdn.purge_token and cdn.zone_id required for provider=cloudflare")
	}
	if c.CDN.Provider == "fastly" && (c.CDN.PurgeToken == "" || c.CDN.FastlyServiceID == "") {
		return fmt.Errorf("config: cdn.purge_token and cdn.fastly_service_id required for provider=fastly")
	}
	if c.CDN.TTLPending <= 0 || c.CDN.TTLApproved <= 0 || c.CDN.TTLApprovedCDN <= 0 || c.CDN.TTLNegative <= 0 {
		return fmt.Errorf("config: cdn TTLs must be positive")
	}
	switch c.Log.Format {
	case "", "json", "text":
	default:
		return fmt.Errorf("config: log.format must be json or text")
	}
	switch c.Log.ClientIPLog {
	case "", "none", "trunc24", "full":
	default:
		return fmt.Errorf("config: log.client_ip_log must be none, trunc24 or full")
	}
	if c.DefaultAvatr.Style == "" {
		c.DefaultAvatr.Style = "identicon"
	}
	if c.DefaultAvatr.RetroStyle == "" {
		c.DefaultAvatr.RetroStyle = "pixel-art"
	}
	return nil
}
