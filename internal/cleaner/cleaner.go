// Package cleaner implements the periodic maintenance loop of SPEC §10.3:
// TTL expiry, negative-entry backoff retries, stale pending re-enqueue, LRU
// capacity control and startup orphan-blob scanning.
package cleaner

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/liueic/avatar/internal/cache"
	"github.com/liueic/avatar/internal/config"
	"github.com/liueic/avatar/internal/metrics"
	"github.com/liueic/avatar/internal/moderate"
)

// FetchRetrier proactively re-fetches expired network-error negatives
// (SPEC §6.3). It is wired to the server's fetch pipeline by main.
type FetchRetrier interface {
	RetryFetch(ctx context.Context, hash string)
}

// Cleaner runs maintenance on a ticker.
type Cleaner struct {
	cfg   config.Config
	store *cache.Store
	blobs *cache.Blobs
	queue *moderate.Service
	log   *slog.Logger
	reg   *metrics.Registry
	retry FetchRetrier
}

// New assembles the cleaner.
func New(cfg config.Config, store *cache.Store, blobs *cache.Blobs, queue *moderate.Service, log *slog.Logger, reg *metrics.Registry, retry FetchRetrier) *Cleaner {
	return &Cleaner{cfg: cfg, store: store, blobs: blobs, queue: queue, log: log, reg: reg, retry: retry}
}

// Run blocks until ctx is cancelled, executing a maintenance pass every
// cache.clean_interval (default 10 minutes, SPEC §10.3).
func (c *Cleaner) Run(ctx context.Context) {
	interval := c.cfg.Cache.CleanInterval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.RunOnce(ctx); err != nil {
				c.log.Error("cleaner pass failed", "err", err)
			}
		}
	}
}

// RunOnce executes one maintenance pass.
func (c *Cleaner) RunOnce(ctx context.Context) error {
	now := time.Now().Unix()

	// 1. TTL expiry: approved entries and stale pending_fetch claims. Expired
	// rejected_fetch rows are handled below (retry or delete, SPEC §6.3).
	expired, err := c.store.DeleteExpired(ctx, now,
		cache.StatusApproved, cache.StatusPendingFetch)
	if err != nil {
		return err
	}
	for _, e := range expired {
		if err := c.blobs.Delete(e.BlobPath); err != nil {
			c.log.Warn("cleaner: delete expired blob", "hash", e.Hash, "err", err)
		}
	}
	if len(expired) > 0 {
		c.reg.Counter("avater_cleaner_expired_total", "Entries removed by the cleaner", nil, float64(len(expired)))
		c.log.Debug("cleaner: expired entries removed", "count", len(expired))
	}

	// 2. Expired negatives: "not_found" rows are dropped (a future request
	// simply re-fetches); network-error rows are proactively retried with
	// exponential backoff, bounded per pass (SPEC §6.3).
	negatives, err := c.store.ExpiredByStatus(ctx, cache.StatusRejectedFetch, now, 50)
	if err != nil {
		c.log.Error("cleaner: negative scan", "err", err)
	}
	for _, e := range negatives {
		if e.FailReason == "not_found" {
			if _, err := c.store.Delete(ctx, e.Hash); err != nil && !errors.Is(err, cache.ErrNotFound) {
				c.log.Warn("cleaner: drop expired negative", "hash", e.Hash, "err", err)
			}
			continue
		}
		if c.retry != nil {
			c.retry.RetryFetch(ctx, e.Hash)
		}
	}

	// 3. Rejected blobs past their retention: delete blob, keep the record
	// (SPEC §10.3 — prevents blind re-fetching of violating images).
	kept, err := c.store.ExpireKeepRecord(ctx, now)
	if err != nil {
		c.log.Error("cleaner: rejected retention pass", "err", err)
	}
	for _, e := range kept {
		_ = c.blobs.Delete(e.BlobPath)
	}
	if len(kept) > 0 {
		c.log.Info("cleaner: dropped rejected blobs past retention", "count", len(kept))
	}

	// 4. Review backlog self-healing: pending_review entries older than the
	// stale window go back into the queue (SPEC §9.3/§10.3).
	staleTs := now - int64(c.cfg.Cache.PendingStale.Seconds())
	stale, err := c.store.PendingOlderThan(ctx, cache.StatusPendingReview, staleTs, 500)
	if err != nil {
		c.log.Error("cleaner: stale pending scan", "err", err)
	}
	if len(stale) > 0 {
		n := c.queue.EnqueueBatch(stale)
		c.log.Warn("cleaner: re-enqueued stale pending_review entries", "found", len(stale), "enqueued", n)
		c.reg.Counter("avater_cleaner_reenqueued_total", "Stale pending_review entries re-enqueued", nil, float64(len(stale)))
	}

	// 5. Capacity control: evict least-recently-accessed approved entries
	// until usage drops to 90% of the budget (SPEC §10.3).
	approvedBytes, err := c.store.ApprovedBytes(ctx)
	if err != nil {
		c.log.Error("cleaner: cache bytes", "err", err)
		return nil
	}
	c.reg.Gauge("avater_cache_approved_bytes", "Bytes stored in approved entries", nil, float64(approvedBytes))
	if approvedBytes > c.cfg.Cache.MaxBytes {
		target := c.cfg.Cache.MaxBytes * 9 / 10
		victims, err := c.store.LRUVictims(ctx, approvedBytes-target)
		if err != nil {
			c.log.Error("cleaner: lru eviction", "err", err)
		}
		for _, v := range victims {
			_ = c.blobs.Delete(v.BlobPath)
		}
		if len(victims) > 0 {
			c.log.Warn("cleaner: LRU eviction", "count", len(victims), "bytes_before", approvedBytes)
			c.reg.Counter("avater_cleaner_evicted_total", "Entries evicted by LRU capacity control", nil, float64(len(victims)))
		}
	}

	// 6. Disk cache bounds for defaults and scaled variants (abuse resilience).
	if err := c.PruneDefaultCache(); err != nil {
		c.log.Warn("cleaner: defaults prune", "err", err)
	}
	if err := c.PruneScaledCache(); err != nil {
		c.log.Warn("cleaner: scaled prune", "err", err)
	}

	// 7. SQLite housekeeping.
	if err := c.store.Optimize(ctx); err != nil {
		c.log.Debug("cleaner: pragma optimize", "err", err)
	}
	return nil
}

// OrphanScan deletes blob files with no DB record; run once at startup
// (SPEC §10.3).
func (c *Cleaner) OrphanScan(ctx context.Context) error {
	onDisk, err := c.blobs.Scan()
	if err != nil {
		return err
	}
	known, err := c.store.KnownBlobPaths(ctx)
	if err != nil {
		return err
	}
	orphans := 0
	for rel := range onDisk {
		if _, ok := known[rel]; !ok {
			if err := c.blobs.Delete(rel); err != nil {
				c.log.Warn("orphan scan: delete", "path", rel, "err", err)
				continue
			}
			orphans++
		}
	}
	if orphans > 0 {
		c.log.Info("orphan scan: removed unreferenced blobs", "count", orphans)
		c.reg.Counter("avater_cleaner_orphans_total", "Orphan blobs removed at startup", nil, float64(orphans))
	}
	return nil
}

// PruneDefaultCache bounds the defaults disk cache ({cache.dir}/defaults):
// when it exceeds default_avatar.disk_max_bytes, oldest-mtime files are
// removed until usage drops to 90% of the cap (abuse resilience: random-hash
// PNG floods would otherwise fill the disk).
func (c *Cleaner) PruneDefaultCache() error {
	limit := c.cfg.DefaultAvatr.DiskMaxBytes
	if limit <= 0 {
		limit = 256 << 20
	}
	total, removed, err := pruneDir(filepath.Join(c.cfg.Cache.Dir, "defaults"), limit)
	if removed > 0 {
		c.log.Warn("cleaner: pruned default-avatar disk cache", "files", removed, "bytes_left", total)
		c.reg.Counter("avater_cleaner_defaults_pruned_total", "Default-avatar cache files pruned", nil, float64(removed))
	}
	c.reg.Gauge("avater_defaults_cache_bytes", "Bytes cached for default avatars on disk", nil, float64(total))
	return err
}

// PruneScaledCache bounds the scaled-variant disk cache ({cache.dir}/scaled)
// at cache.scaled_disk_max_bytes (LRU by mtime).
func (c *Cleaner) PruneScaledCache() error {
	limit := c.cfg.Cache.ScaledDiskMaxBytes
	if limit <= 0 {
		limit = 512 << 20
	}
	total, removed, err := pruneDir(filepath.Join(c.cfg.Cache.Dir, "scaled"), limit)
	if removed > 0 {
		c.log.Warn("cleaner: pruned scaled-variant disk cache", "files", removed, "bytes_left", total)
		c.reg.Counter("avater_cleaner_scaled_pruned_total", "Scaled-variant cache files pruned", nil, float64(removed))
	}
	c.reg.Gauge("avater_scaled_cache_bytes", "Bytes cached for scaled avatar variants on disk", nil, float64(total))
	return err
}

// pruneDir removes oldest-mtime files until the directory total is at 90% of
// limit. Returns the resulting total and removed file count.
func pruneDir(root string, limit int64) (total int64, removed int, err error) {
	type file struct {
		path  string
		size  int64
		mtime time.Time
	}
	var files []file
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil //nolint: walk errors on a cache dir are non-fatal
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		files = append(files, file{path, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if walkErr != nil {
		return total, removed, walkErr
	}
	if total <= limit {
		return total, removed, nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	target := limit * 9 / 10
	for _, f := range files {
		if total <= target {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
			removed++
		}
	}
	return total, removed, nil
}
