// Command avater runs the self-hosted Gravatar-compatible avatar proxy
// (SPEC: see SPEC.md at the repository root).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/liueic/avatar/internal/admin"
	"github.com/liueic/avatar/internal/avatar"
	"github.com/liueic/avatar/internal/cache"
	"github.com/liueic/avatar/internal/cdn"
	"github.com/liueic/avatar/internal/cleaner"
	"github.com/liueic/avatar/internal/config"
	"github.com/liueic/avatar/internal/fetcher"
	"github.com/liueic/avatar/internal/metrics"
	"github.com/liueic/avatar/internal/moderate"
	"github.com/liueic/avatar/internal/moderate/onnx"
	"github.com/liueic/avatar/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "avater: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "", "path to avater.toml (defaults to $AVATER_CONFIG)")
	healthcheck := flag.Bool("healthcheck", false, "probe /readyz and exit (Docker HEALTHCHECK)")
	flag.Parse()

	if *healthcheck {
		url := os.Getenv("AVATER_HEALTHCHECK_URL")
		if url == "" {
			url = "http://127.0.0.1:8080/readyz"
		}
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(url)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("healthcheck: %s returned %d", url, resp.StatusCode)
		}
		return nil
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	log := newLogger(cfg)
	reg := metrics.New()

	// Storage.
	store, err := cache.Open(cfg.Cache.Dir + "/avater.db")
	if err != nil {
		return err
	}
	defer store.Close()
	blobs, err := cache.NewBlobs(cfg.Cache.Dir)
	if err != nil {
		return err
	}

	// Default-avatar generator (DiceBear, deterministic — SPEC §8).
	avatars, err := avatar.NewGenerator(cfg.Cache.Dir, cfg.DefaultAvatr.Style, cfg.DefaultAvatr.RetroStyle, cfg.Cache.DefaultLRU, cfg.DefaultAvatr.MaxRasterSize)
	if err != nil {
		return err
	}

	// Upstream fetcher (SSRF-hardened — SPEC §6).
	fetch, err := fetcher.New(fetcher.Config{
		AllowedHosts: cfg.Upstream.AllowedHosts,
		MaxBytes:     cfg.Upstream.MaxBytes,
		Timeout:      cfg.Upstream.Timeout,
		DialTimeout:  cfg.Upstream.DialTimeout,
		TLSTimeout:   cfg.Upstream.TLSTimeout,
	}, fetcher.Options{})
	if err != nil {
		return err
	}
	upTB := fetcher.NewTokenBucket(cfg.Upstream.RateLimitRPS, cfg.Upstream.RateLimitBurst)

	// CDN header policy + purge channel (SPEC §16).
	policy := cdn.NewPolicy(cfg.CDN)
	purger := cdn.NewPurger(cfg.CDN, log, reg)

	// Moderation queue/workers (SPEC §9).
	queue := moderate.NewService(store, blobs, moderate.ServiceConfig{
		Workers:           cfg.Moderation.Workers,
		QueueSize:         cfg.Moderation.QueueSize,
		MinInterval:       cfg.Moderation.MinInterval,
		InferenceTimeout:  cfg.Moderation.InferenceTimeout,
		MaxAttempts:       cfg.Moderation.MaxAttempts,
		ThresholdNSFW:     cfg.Moderation.ThresholdNSFW,
		ThresholdNSFL:     cfg.Moderation.ThresholdNSFL,
		GrayZoneThreshold: cfg.Moderation.GrayZoneThreshold,
		GrayZoneAction:    cfg.Moderation.GrayZoneAction,
		TTLApproved:       cfg.Cache.TTLApproved,
		TTLRejected:       cfg.Cache.TTLRejected,
	}, log, reg, purger)
	queue.Start()

	srv := server.New(server.Deps{
		Cfg:     cfg,
		Store:   store,
		Blobs:   blobs,
		Avatars: avatars,
		Fetch:   fetch,
		UpTB:    upTB,
		Queue:   queue,
		Policy:  policy,
		Log:     log,
		Reg:     reg,
	})

	// Cleaner (SPEC §10.3).
	cl := cleaner.New(cfg, store, blobs, queue, log, reg, srv)
	if err := cl.OrphanScan(context.Background()); err != nil {
		log.Warn("orphan blob scan failed", "err", err)
	}
	cleanerCtx, stopCleaner := context.WithCancel(context.Background())
	go cl.Run(cleanerCtx)

	// Moderation engine: none is instant; onnx loads in the background while
	// /avatar keeps serving defaults and /readyz reports 503 (SPEC §14).
	engineReady := make(chan struct{})
	go func() {
		defer close(engineReady)
		var mod moderate.ModeratorWithVersion
		switch cfg.Moderation.Engine {
		case "none":
			mod = moderate.NewNone(moderate.NonePolicy(cfg.Moderation.NonePolicy))
		case "onnx":
			m, err := onnx.New(onnx.Config{
				ModelPath:   cfg.Moderation.ModelPath,
				ModelSHA256: cfg.Moderation.ModelSHA256,
				ORTLibPath:  cfg.Moderation.ORTLibPath,
			})
			if err != nil {
				// Moderation unavailable: every fetched image stays
				// pending_review and URLs serve the default avatar (SPEC §9.3
				// degradation path). The operator must fix config/models.
				log.Error("moderation engine unavailable; serving defaults until restarted", "err", err)
				reg.Counter("avater_moderation_engine_failures_total", "Moderation engine initialization failures", nil, 1)
				return
			}
			mod = m
		}
		queue.SetModerator(mod)
		if err := queue.Bootstrap(context.Background()); err != nil {
			log.Error("moderation bootstrap scan failed", "err", err)
		}
		n, err := queue.CheckModelUpgrade(context.Background(), cfg.Moderation.ReReviewOnModelUpgrade)
		if err != nil {
			log.Error("model-upgrade re-review failed", "err", err)
		} else if n > 0 {
			log.Info("model upgrade: re-review scheduled", "entries", n)
		}
		log.Info("moderation engine ready", "engine", cfg.Moderation.Engine, "model_ver", queue.EngineStatus())
	}()

	// Public HTTP server (SPEC §4).
	public := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		log.Info("avater listening", "listen", cfg.Listen, "engine", cfg.Moderation.Engine)
		errCh <- public.ListenAndServe()
	}()

	// Admin server on its own port (SPEC §12) — never expose publicly.
	var adminSrv *http.Server
	if cfg.AdminListen != "" {
		adminSrv = &http.Server{
			Addr: cfg.AdminListen,
			Handler: admin.New(admin.Deps{
				Cfg:    cfg,
				Store:  store,
				Blobs:  blobs,
				Queue:  queue,
				Purger: purger,
				Log:    log,
				Reg:    reg,
				Health: func() admin.HealthInfo {
					info := admin.HealthInfo{
						Engine:        cfg.Moderation.Engine,
						ModelVer:      queue.EngineStatus(),
						Ready:         queue.ModeratorReady(),
						LastInference: queue.LastInference().String(),
					}
					if cfg.Moderation.Engine == "onnx" && queue.ModeratorReady() {
						info.ORTVersion = onnx.Version()
					}
					return info
				},
			}).Handler(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
		}
		go func() {
			log.Info("admin listening", "listen", cfg.AdminListen)
			errCh <- adminSrv.ListenAndServe()
		}()
	}

	// Graceful shutdown (SPEC §3: 优雅退出).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Info("shutting down", "signal", s.String())
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			stopCleaner()
			queue.Stop()
			purger.Stop()
			upTB.Stop()
			return err
		}
	}

	shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = public.Shutdown(shCtx)
	if adminSrv != nil {
		_ = adminSrv.Shutdown(shCtx)
	}
	stopCleaner()
	queue.Stop()
	purger.Stop()
	upTB.Stop()
	return nil
}

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Log.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Log.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
