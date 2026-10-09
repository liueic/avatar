package moderate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	_ "image/gif"  // register decoders
	_ "image/jpeg" // register decoders
	_ "image/png"  // register decoders
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	_ "golang.org/x/image/webp" // register decoder (first frame only)

	"github.com/liueic/avatar/internal/cache"
	"github.com/liueic/avatar/internal/metrics"
)

// Purger receives hash tags whose CDN-cached content changed (SPEC §16.3).
// Implementations must be asynchronous and never block the caller.
type Purger interface {
	PurgeTagsAsync(hashes ...string)
}

// NOPPurger is the default no-op purger.
type NOPPurger struct{}

// PurgeTagsAsync implements Purger.
func (NOPPurger) PurgeTagsAsync(hashes ...string) {}

// ModelVersioner is implemented by engines that can report their model
// version (used by the model-upgrade re-review, SPEC §10.4).
type ModelVersioner interface {
	ModelVersion() string
}

// Moderator now includes ModelVersion so the service can detect upgrades.
// (Embedded here to keep the moderation interface in one place.)
type ModeratorWithVersion interface {
	Moderator
	ModelVersion() string
}

// ServiceConfig carries the tunables the queue needs from config.
type ServiceConfig struct {
	Workers           int
	QueueSize         int
	MinInterval       time.Duration
	InferenceTimeout  time.Duration
	MaxAttempts       int
	ThresholdNSFW     float64
	ThresholdNSFL     float64
	GrayZoneThreshold float64
	GrayZoneAction    string
	TTLApproved       time.Duration
	TTLRejected       time.Duration
}

// Service wires the review queue, worker pool and persistence together.
type Service struct {
	store *cache.Store
	blobs *cache.Blobs
	cfg   ServiceConfig
	log   *slog.Logger
	reg   *metrics.Registry
	purge Purger

	queue chan string
	stop  chan struct{}
	wg    sync.WaitGroup

	mu                  sync.Mutex
	mod                 ModeratorWithVersion
	consecutiveFailures int
	lastInference       atomic.Int64 // unix nanos of the last successful classify

	attemptsMu sync.Mutex
	attempts   map[string]int
}

// NewService assembles the queue. Call Start after SetModerator (or before —
// workers idle until an engine is installed).
func NewService(store *cache.Store, blobs *cache.Blobs, cfg ServiceConfig, log *slog.Logger, reg *metrics.Registry, purge Purger) *Service {
	if purge == nil {
		purge = NOPPurger{}
	}
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 3
	}
	if cfg.MinInterval < 0 {
		cfg.MinInterval = 0
	}
	return &Service{
		store:    store,
		blobs:    blobs,
		cfg:      cfg,
		log:      log,
		reg:      reg,
		purge:    purge,
		queue:    make(chan string, cfg.QueueSize),
		stop:     make(chan struct{}),
		attempts: make(map[string]int),
	}
}

// SetModerator installs the review engine. Workers pick it up on their next
// loop iteration.
func (s *Service) SetModerator(m ModeratorWithVersion) {
	s.mu.Lock()
	s.mod = m
	s.mu.Unlock()
}

// ModeratorReady reports whether an engine is installed.
func (s *Service) ModeratorReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mod != nil
}

// EngineStatus reports the engine for the admin health endpoint.
func (s *Service) EngineStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mod == nil {
		return "loading"
	}
	if mv, ok := s.mod.(ModelVersioner); ok {
		return mv.ModelVersion()
	}
	return "ready"
}

// Depth returns the current queue length.
func (s *Service) Depth() int { return len(s.queue) }

// LastInference returns the duration of the most recent successful
// classification (admin health, SPEC §12).
func (s *Service) LastInference() time.Duration {
	return time.Duration(s.lastInference.Load())
}

// Start launches the worker goroutines.
func (s *Service) Start() {
	for i := 0; i < s.cfg.Workers; i++ {
		s.wg.Add(1)
		go s.worker(i)
	}
}

// Stop halts the workers. Queued hashes stay persisted as pending_review and
// are re-enqueued by the next startup scan.
func (s *Service) Stop() {
	close(s.stop)
	s.wg.Wait()
}

// Enqueue schedules a hash for review without ever blocking (SPEC §9.3:
// overflow stays in the DB until the cleaner / startup scan picks it up).
func (s *Service) Enqueue(hash string) {
	select {
	case s.queue <- hash:
	default:
		s.reg.Counter("avater_queue_overflow_total", "Review jobs dropped because the queue was full", nil, 1)
		s.log.Warn("moderation queue full; entry awaits cleaner re-enqueue", "hash", hash)
	}
}

// EnqueueBatch enqueues many hashes; returns the number admitted to the
// in-memory queue (the rest remain pending_review in the DB).
func (s *Service) EnqueueBatch(hashes []string) int {
	n := 0
	for _, h := range hashes {
		select {
		case s.queue <- h:
			n++
		default:
			return n
		}
	}
	return n
}

// Bootstrap re-enqueues every pending_review entry after a restart (SPEC
// §9.3: the persisted record is the source of truth; queue loss loses
// nothing).
func (s *Service) Bootstrap(ctx context.Context) error {
	hashes, err := s.store.PendingOlderThan(ctx, cache.StatusPendingReview, 0, s.cfg.QueueSize)
	if err != nil {
		return err
	}
	n := s.EnqueueBatch(hashes)
	s.log.Info("moderation bootstrap scan", "pending", len(hashes), "enqueued", n)
	return nil
}

func (s *Service) worker(id int) {
	defer s.wg.Done()
	for {
		select {
		case <-s.stop:
			return
		case hash := <-s.queue:
			ctx := context.Background()
			s.process(ctx, hash)
			if s.cfg.MinInterval > 0 {
				time.Sleep(s.cfg.MinInterval) // pacing, SPEC §9.3
			}
		}
	}
}

// process reviews one pending_review entry end to end.
func (s *Service) process(ctx context.Context, hash string) {
	entry, err := s.store.Get(ctx, hash)
	if err != nil {
		if errors.Is(err, cache.ErrNotFound) {
			return
		}
		s.log.Error("moderation: load entry", "hash", hash, "err", err)
		return
	}
	if entry.Status != cache.StatusPendingReview {
		return // already handled (manual override, delete, ...)
	}

	img, err := s.loadImage(entry)
	if err != nil {
		s.log.Error("moderation: load image", "hash", hash, "err", err)
		s.reg.Counter("avater_moderation_failures_total", "Review attempts that could not run", map[string]string{"stage": "load"}, 1)
		return // left pending_review; the stale sweep retries after 24h
	}

	mod := s.current()
	if mod == nil {
		// Engine not ready: put the job back at the tail (bounded by
		// MaxAttempts in-process; the DB sweep is the durable fallback).
		s.requeueAtTail(hash)
		return
	}

	var res Result
	for attempt := 1; ; attempt++ {
		ictx, cancel := context.WithTimeout(ctx, s.cfg.InferenceTimeout)
		res, err = mod.Classify(ictx, img)
		cancel()
		if err == nil {
			break
		}
		s.reg.Counter("avater_moderation_failures_total", "Review attempts that could not run", map[string]string{"stage": "inference"}, 1)
		if attempt >= s.cfg.MaxAttempts {
			s.mu.Lock()
			s.consecutiveFailures++
			cf := s.consecutiveFailures
			s.mu.Unlock()
			if cf == 1 || cf%10 == 0 {
				s.log.Error("moderation engine failing repeatedly", "hash", hash, "consecutive", cf, "err", err)
			}
			s.reg.Gauge("avater_moderation_consecutive_failures", "Consecutive moderation engine failures", nil, float64(cf))
			s.requeueAtTail(hash)
			return
		}
		time.Sleep(time.Duration(1<<(attempt-1)) * time.Second) // 1s, 2s, 4s...
	}

	s.mu.Lock()
	s.consecutiveFailures = 0
	s.mu.Unlock()
	s.reg.Gauge("avater_moderation_consecutive_failures", "Consecutive moderation engine failures", nil, 0)
	s.reg.Histogram("avater_moderation_duration_seconds", "Time to classify one image", nil, nil, res.Duration.Seconds())

	dec := Decide(res.Scores, s.cfg.ThresholdNSFW, s.cfg.ThresholdNSFL, s.cfg.GrayZoneThreshold, s.cfg.GrayZoneAction)
	scoresJSON, _ := json.Marshal(res.Scores)

	if err := s.store.MarkReviewed(ctx, hash, res.ModelVer, string(scoresJSON), dec.Approved, dec.GrayZone, false, s.cfg.TTLApproved, s.cfg.TTLRejected); err != nil {
		switch {
		case errors.Is(err, cache.ManualOverrideError):
			s.log.Info("moderation: keeping manual decision", "hash", hash)
		case errors.Is(err, cache.ErrNotFound):
			// deleted meanwhile
		default:
			s.log.Error("moderation: persist verdict", "hash", hash, "err", err)
		}
		return
	}

	s.forgetAttempts(hash)
	s.lastInference.Store(int64(res.Duration))
	s.reg.Counter("avater_moderation_verdicts_total", "Moderation verdicts applied", map[string]string{"verdict": dec.Verdict.String()}, 1)
	s.log.Info("moderation verdict",
		"hash", hash,
		"status", verdictStatus(dec.Approved),
		"verdict", dec.Verdict.String(),
		"gray_zone", dec.GrayZone,
		"model", res.ModelVer,
		"latency", res.Duration.String(),
	)
	// The served content for this URL may change with the verdict; make the
	// CDN drop any cached copies promptly (SPEC §16.3).
	s.purge.PurgeTagsAsync(hash)
}

// loadImage reads and decodes the stored blob. Animated inputs resolve to
// their first frame, matching SPEC §7.4 (jpeg/png/gif/webp are all registered
// decoders; webp decodes its first frame).
func (s *Service) loadImage(e *cache.Entry) (image.Image, error) {
	data, err := s.blobs.Read(e.BlobPath)
	if err != nil {
		return nil, err
	}
	im, _, err := image.Decode(bytes.NewReader(data))
	return im, err
}

func (s *Service) current() ModeratorWithVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mod
}

// requeueAtTail implements "stay at the queue tail + alert count" for
// transient failures (SPEC §9.2), bounded by MaxAttempts before the durable
// pending_review sweep takes over.
func (s *Service) requeueAtTail(hash string) {
	s.attemptsMu.Lock()
	n := s.attempts[hash] + 1
	s.attempts[hash] = n
	over := n >= s.cfg.MaxAttempts
	if over {
		delete(s.attempts, hash)
	}
	s.attemptsMu.Unlock()
	if !over {
		s.Enqueue(hash)
	}
}

func (s *Service) forgetAttempts(hash string) {
	s.attemptsMu.Lock()
	delete(s.attempts, hash)
	s.attemptsMu.Unlock()
}

// CheckModelUpgrade requeues entries whose recorded model_ver differs from
// the running engine's version (SPEC §10.4). Returns the number requeued.
func (s *Service) CheckModelUpgrade(ctx context.Context, enabled bool) (int, error) {
	mod := s.current()
	if mod == nil || !enabled {
		return 0, nil
	}
	current := mod.ModelVersion()
	if current == "" {
		return 0, nil
	}
	const batch = 500
	total := 0
	for {
		hashes, err := s.store.RequeueStaleModelVer(ctx, current, batch)
		if err != nil {
			return total, err
		}
		if len(hashes) == 0 {
			return total, nil
		}
		s.EnqueueBatch(hashes)
		total += len(hashes)
		if len(hashes) < batch {
			return total, nil
		}
	}
}

func verdictStatus(approved bool) string {
	if approved {
		return "approved"
	}
	return "rejected"
}
