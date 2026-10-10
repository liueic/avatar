package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// applyEnv overlays AVATER_* environment variables on top of the current
// values. Environment variables take precedence over the TOML file (SPEC §13).
func (c *Config) applyEnv() error {
	str := func(name string, dst *string) {
		if v, ok := os.LookupEnv(name); ok {
			*dst = v
		}
	}
	dur := func(name string, dst *time.Duration) error {
		v, ok := os.LookupEnv(name)
		if !ok {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("config: %s: %w", name, err)
		}
		*dst = d
		return nil
	}
	i64 := func(name string, dst *int64) error {
		v, ok := os.LookupEnv(name)
		if !ok {
			return nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("config: %s: %w", name, err)
		}
		*dst = n
		return nil
	}
	integer := func(name string, dst *int) error {
		v, ok := os.LookupEnv(name)
		if !ok {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: %s: %w", name, err)
		}
		*dst = n
		return nil
	}
	f64 := func(name string, dst *float64) error {
		v, ok := os.LookupEnv(name)
		if !ok {
			return nil
		}
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("config: %s: %w", name, err)
		}
		*dst = n
		return nil
	}
	boolean := func(name string, dst *bool) error {
		v, ok := os.LookupEnv(name)
		if !ok {
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: %s: %w", name, err)
		}
		*dst = b
		return nil
	}

	str("AVATER_LISTEN", &c.Listen)
	str("AVATER_ADMIN_LISTEN", &c.AdminListen)
	str("AVATER_ADMIN_TOKEN", &c.AdminToken)

	str("AVATER_LOG_FORMAT", &c.Log.Format)
	str("AVATER_LOG_LEVEL", &c.Log.Level)
	if err := boolean("AVATER_LOG_ACCESS", &c.Log.AccessLog); err != nil {
		return err
	}
	str("AVATER_LOG_CLIENT_IP", &c.Log.ClientIPLog)

	if err := boolean("AVATER_RATELIMIT_ENABLED", &c.RateLimit.Enabled); err != nil {
		return err
	}
	if err := f64("AVATER_RATELIMIT_GLOBAL_RPS", &c.RateLimit.GlobalRPS); err != nil {
		return err
	}
	if err := integer("AVATER_RATELIMIT_GLOBAL_BURST", &c.RateLimit.GlobalBurst); err != nil {
		return err
	}
	if err := f64("AVATER_RATELIMIT_PER_IP_RPS", &c.RateLimit.PerIPRPS); err != nil {
		return err
	}
	if err := integer("AVATER_RATELIMIT_PER_IP_BURST", &c.RateLimit.PerIPBurst); err != nil {
		return err
	}

	if v, ok := os.LookupEnv("AVATER_UPSTREAM_ALLOWED_HOSTS"); ok {
		hosts := strings.Split(v, ",")
		c.Upstream.AllowedHosts = make([]string, 0, len(hosts))
		for _, h := range hosts {
			if h = strings.TrimSpace(strings.ToLower(h)); h != "" {
				c.Upstream.AllowedHosts = append(c.Upstream.AllowedHosts, h)
			}
		}
	}
	if err := i64("AVATER_UPSTREAM_MAX_BYTES", &c.Upstream.MaxBytes); err != nil {
		return err
	}
	if err := dur("AVATER_UPSTREAM_TIMEOUT", &c.Upstream.Timeout); err != nil {
		return err
	}
	if err := dur("AVATER_UPSTREAM_DIAL_TIMEOUT", &c.Upstream.DialTimeout); err != nil {
		return err
	}
	if err := dur("AVATER_UPSTREAM_TLS_TIMEOUT", &c.Upstream.TLSTimeout); err != nil {
		return err
	}
	if err := f64("AVATER_UPSTREAM_RATE_LIMIT_RPS", &c.Upstream.RateLimitRPS); err != nil {
		return err
	}
	if err := integer("AVATER_UPSTREAM_RATE_LIMIT_BURST", &c.Upstream.RateLimitBurst); err != nil {
		return err
	}
	if err := integer("AVATER_UPSTREAM_FETCH_SIZE", &c.Upstream.FetchSize); err != nil {
		return err
	}
	if err := dur("AVATER_UPSTREAM_TOKEN_WAIT", &c.Upstream.TokenWait); err != nil {
		return err
	}

	if err := integer("AVATER_VALIDATE_MAX_DIMENSION", &c.Validate.MaxDimension); err != nil {
		return err
	}
	if err := i64("AVATER_VALIDATE_MAX_PIXELS", &c.Validate.MaxPixels); err != nil {
		return err
	}
	if err := boolean("AVATER_VALIDATE_REENCODE", &c.Validate.Reencode); err != nil {
		return err
	}
	if err := integer("AVATER_VALIDATE_JPEG_QUALITY", &c.Validate.JPEGQuality); err != nil {
		return err
	}

	str("AVATER_MODERATION_ENGINE", &c.Moderation.Engine)
	str("AVATER_MODERATION_NONE_POLICY", &c.Moderation.NonePolicy)
	str("AVATER_MODERATION_MODEL_PATH", &c.Moderation.ModelPath)
	str("AVATER_MODERATION_MODEL_SHA256", &c.Moderation.ModelSHA256)
	str("AVATER_MODERATION_ORT_LIB_PATH", &c.Moderation.ORTLibPath)
	if err := integer("AVATER_MODERATION_WORKERS", &c.Moderation.Workers); err != nil {
		return err
	}
	if err := integer("AVATER_MODERATION_QUEUE_SIZE", &c.Moderation.QueueSize); err != nil {
		return err
	}
	if err := dur("AVATER_MODERATION_MIN_INTERVAL", &c.Moderation.MinInterval); err != nil {
		return err
	}
	if err := dur("AVATER_MODERATION_INFERENCE_TIMEOUT", &c.Moderation.InferenceTimeout); err != nil {
		return err
	}
	if err := integer("AVATER_MODERATION_MAX_ATTEMPTS", &c.Moderation.MaxAttempts); err != nil {
		return err
	}
	if err := f64("AVATER_MODERATION_THRESHOLD_NSFW", &c.Moderation.ThresholdNSFW); err != nil {
		return err
	}
	if err := f64("AVATER_MODERATION_THRESHOLD_NSFL", &c.Moderation.ThresholdNSFL); err != nil {
		return err
	}
	str("AVATER_MODERATION_GRAY_ZONE_ACTION", &c.Moderation.GrayZoneAction)
	if err := f64("AVATER_MODERATION_GRAY_ZONE_THRESHOLD", &c.Moderation.GrayZoneThreshold); err != nil {
		return err
	}
	if err := boolean("AVATER_MODERATION_RE_REVIEW_ON_UPGRADE", &c.Moderation.ReReviewOnModelUpgrade); err != nil {
		return err
	}

	str("AVATER_CACHE_DIR", &c.Cache.Dir)
	if err := i64("AVATER_CACHE_MAX_BYTES", &c.Cache.MaxBytes); err != nil {
		return err
	}
	if err := dur("AVATER_CACHE_TTL_APPROVED", &c.Cache.TTLApproved); err != nil {
		return err
	}
	if err := dur("AVATER_CACHE_TTL_NEGATIVE", &c.Cache.TTLNegative); err != nil {
		return err
	}
	if err := dur("AVATER_CACHE_TTL_NEGATIVE_404", &c.Cache.TTLNegative404); err != nil {
		return err
	}
	if err := dur("AVATER_CACHE_TTL_REJECTED", &c.Cache.TTLRejected); err != nil {
		return err
	}
	if err := dur("AVATER_CACHE_PENDING_STALE", &c.Cache.PendingStale); err != nil {
		return err
	}
	if err := dur("AVATER_CACHE_CLEAN_INTERVAL", &c.Cache.CleanInterval); err != nil {
		return err
	}
	if err := integer("AVATER_CACHE_DEFAULT_LRU", &c.Cache.DefaultLRU); err != nil {
		return err
	}
	if err := i64("AVATER_CACHE_MAX_NEGATIVE_ENTRIES", &c.Cache.MaxNegativeEntries); err != nil {
		return err
	}
	if err := i64("AVATER_CACHE_SCALED_DISK_MAX_BYTES", &c.Cache.ScaledDiskMaxBytes); err != nil {
		return err
	}

	str("AVATER_CDN_PROVIDER", &c.CDN.Provider)
	if err := boolean("AVATER_CDN_CACHE_TAG", &c.CDN.CacheTag); err != nil {
		return err
	}
	str("AVATER_CDN_PURGE_TOKEN", &c.CDN.PurgeToken)
	str("AVATER_CDN_ZONE_ID", &c.CDN.ZoneID)
	str("AVATER_CDN_FASTLY_SERVICE_ID", &c.CDN.FastlyServiceID)
	if err := dur("AVATER_CDN_TTL_PENDING", &c.CDN.TTLPending); err != nil {
		return err
	}
	if err := dur("AVATER_CDN_TTL_APPROVED", &c.CDN.TTLApproved); err != nil {
		return err
	}
	if err := dur("AVATER_CDN_TTL_APPROVED_CDN", &c.CDN.TTLApprovedCDN); err != nil {
		return err
	}
	if err := dur("AVATER_CDN_TTL_NEGATIVE", &c.CDN.TTLNegative); err != nil {
		return err
	}
	if err := dur("AVATER_CDN_SWR", &c.CDN.SWR); err != nil {
		return err
	}
	if err := dur("AVATER_CDN_SIE", &c.CDN.SIE); err != nil {
		return err
	}

	str("AVATER_DEFAULT_AVATAR_STYLE", &c.DefaultAvatr.Style)
	str("AVATER_DEFAULT_AVATAR_RETRO_STYLE", &c.DefaultAvatr.RetroStyle)
	if err := integer("AVATER_DEFAULT_AVATAR_MAX_RASTER_SIZE", &c.DefaultAvatr.MaxRasterSize); err != nil {
		return err
	}
	if err := i64("AVATER_DEFAULT_AVATAR_DISK_MAX_BYTES", &c.DefaultAvatr.DiskMaxBytes); err != nil {
		return err
	}

	if err := boolean("AVATER_METRICS_ENABLED", &c.Metrics.Enabled); err != nil {
		return err
	}
	return nil
}
