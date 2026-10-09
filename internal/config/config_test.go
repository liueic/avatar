package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsPassValidation(t *testing.T) {
	c := Default()
	c.AdminToken = "required-by-spec" // SPEC §13: token must be non-empty
	if err := c.Check(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	if c.Listen != ":8080" || c.AdminListen != ":8081" {
		t.Fatalf("unexpected listen defaults: %s %s", c.Listen, c.AdminListen)
	}
	if c.Moderation.Workers != 1 || c.Moderation.Workers > 2 {
		t.Fatalf("workers default = %d, want 1 (SPEC §13)", c.Moderation.Workers)
	}
	if c.Upstream.Timeout != 5*time.Second {
		t.Fatalf("upstream timeout default = %v", c.Upstream.Timeout)
	}
}

func TestAdminTokenRequired(t *testing.T) {
	c := Default()
	c.AdminToken = ""
	if err := c.Check(); err == nil {
		t.Fatal("admin enabled without token must fail (SPEC §13)")
	}
	c.AdminListen = ""
	if err := c.Check(); err != nil {
		t.Fatalf("admin disabled without token should pass: %v", err)
	}
}

func TestWorkersBounded(t *testing.T) {
	c := Default()
	c.AdminToken = "x"
	c.Moderation.Workers = 3
	if err := c.Check(); err == nil {
		t.Fatal("workers > 2 must fail (SPEC §13)")
	}
	c.Moderation.Workers = 0
	if err := c.Check(); err == nil {
		t.Fatal("workers < 1 must fail")
	}
}

func TestLoadTOMLOverriddenByEnv(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "avater.toml")
	content := `
listen = ":9999"
admin_token = "filetoken"

[cache]
dir = "testdata"
max_bytes = 12345678
ttl_approved = "48h"

[moderation]
engine = "none"
none_policy = "approve"
`
	if err := os.WriteFile(tomlPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("AVATER_CONFIG", tomlPath)
	t.Setenv("AVATER_LISTEN", ":7777")             // env beats file
	t.Setenv("AVATER_CACHE_MAX_BYTES", "87654321") // env beats file
	t.Setenv("AVATER_MODERATION_THRESHOLD_NSFW", "0.75")
	t.Setenv("AVATER_UPSTREAM_ALLOWED_HOSTS", "secure.gravatar.com, 1.gravatar.com")
	t.Setenv("AVATER_CDN_TTL_PENDING", "30s")

	c, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Listen != ":7777" {
		t.Errorf("listen = %s, want env override :7777", c.Listen)
	}
	if c.AdminToken != "filetoken" {
		t.Errorf("admin_token = %s, want file value", c.AdminToken)
	}
	if c.Cache.Dir != "testdata" {
		t.Errorf("cache.dir = %s, want file value", c.Cache.Dir)
	}
	if c.Cache.MaxBytes != 87654321 {
		t.Errorf("cache.max_bytes = %d, want env override", c.Cache.MaxBytes)
	}
	if c.Cache.TTLApproved != 48*time.Hour {
		t.Errorf("ttl_approved = %v, want 48h from file", c.Cache.TTLApproved)
	}
	if c.Moderation.Engine != "none" || c.Moderation.NonePolicy != "approve" {
		t.Errorf("moderation engine/policy = %s/%s", c.Moderation.Engine, c.Moderation.NonePolicy)
	}
	if c.Moderation.ThresholdNSFW != 0.75 {
		t.Errorf("threshold_nsfw = %v, want 0.75", c.Moderation.ThresholdNSFW)
	}
	if len(c.Upstream.AllowedHosts) != 2 || c.Upstream.AllowedHosts[1] != "1.gravatar.com" {
		t.Errorf("allowed_hosts = %v", c.Upstream.AllowedHosts)
	}
	if c.CDN.TTLPending != 30*time.Second {
		t.Errorf("cdn ttl_pending = %v", c.CDN.TTLPending)
	}
}

func TestCheckRejectsBadEngine(t *testing.T) {
	c := Default()
	c.AdminToken = "x"
	c.Moderation.Engine = "bogus"
	if err := c.Check(); err == nil {
		t.Fatal("bogus engine must fail")
	}
}
