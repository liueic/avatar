package moderate

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"testing"
	"time"

	"github.com/liueic/avatar/internal/cache"
	"github.com/liueic/avatar/internal/metrics"
)

func TestDecideThresholds(t *testing.T) {
	cases := []struct {
		name     string
		scores   map[string]float32
		wantOK   bool
		wantGray bool
		action   string
	}{
		{"clearly safe", map[string]float32{"SFW": 0.97, "NSFW": 0.02, "NSFL": 0.01}, true, false, "reject"},
		{"nsfw over threshold", map[string]float32{"SFW": 0.2, "NSFW": 0.7, "NSFL": 0.1}, false, false, "reject"},
		{"nsfw at threshold", map[string]float32{"SFW": 0.4, "NSFW": 0.5, "NSFL": 0.1}, false, false, "reject"},
		{"nsfl over threshold", map[string]float32{"SFW": 0.1, "NSFW": 0.1, "NSFL": 0.8}, false, false, "reject"},
		{"gray zone rejects by default", map[string]float32{"SFW": 0.55, "NSFW": 0.25, "NSFL": 0.2}, false, true, "reject"},
		{"gray zone approves when configured", map[string]float32{"SFW": 0.55, "NSFW": 0.25, "NSFL": 0.2}, true, true, "approve"},
		{"all low is gray", map[string]float32{"SFW": 0.3, "NSFW": 0.3, "NSFL": 0.4}, false, true, "reject"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.scores, 0.5, 0.5, 0.6, tc.action)
			if got.Approved != tc.wantOK {
				t.Errorf("approved = %v, want %v (verdict %v)", got.Approved, tc.wantOK, got.Verdict)
			}
			if got.GrayZone != tc.wantGray {
				t.Errorf("gray = %v, want %v", got.GrayZone, tc.wantGray)
			}
		})
	}
}

func TestNoneEngine(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(0, 0, color.White)

	res, err := NewNone(NoneApprove).Classify(context.Background(), img)
	if err != nil || res.Verdict != VerdictSFW {
		t.Fatalf("approve policy: %v %v", res, err)
	}
	if res.ModelVer == "" {
		t.Fatal("model version must be set")
	}

	res, err = NewNone(NoneReject).Classify(context.Background(), img)
	if err != nil || res.Verdict != VerdictNSFW {
		t.Fatalf("reject policy: %v %v", res, err)
	}
	if _, err := NewNone(NoneReject).Classify(context.Background(), nil); err == nil {
		t.Fatal("nil image must error")
	}
}

// fakeMod is a configurable Moderator for worker-flow tests. Classify keys
// off the image pixel color: red -> SFW, anything else -> NSFW, letting one
// instance serve different verdicts per entry.
type fakeMod struct {
	deny bool
}

func (f *fakeMod) Classify(ctx context.Context, img image.Image) (Result, error) {
	if f.deny {
		return Result{}, errors.New("engine down")
	}
	r, _, _, _ := img.At(img.Bounds().Min.X, img.Bounds().Min.Y).RGBA()
	if r > 128 {
		return Result{Verdict: VerdictSFW, Scores: map[string]float32{"SFW": 0.99, "NSFW": 0.005, "NSFL": 0.005}, ModelVer: "fake@1"}, nil
	}
	return Result{Verdict: VerdictNSFW, Scores: map[string]float32{"SFW": 0.1, "NSFW": 0.9, "NSFL": 0.0}, ModelVer: "fake@1"}, nil
}
func (f *fakeMod) Close() error         { return nil }
func (f *fakeMod) ModelVersion() string { return "fake@1" }

const (
	hashOK   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hashNSFW = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// seedPNG encodes a 1x1 PNG of the given color. fakeMod keys its verdict off
// the red channel: red -> SFW, anything else -> NSFW.
func seedPNG(c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, c)
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func newTestService(t *testing.T) (*Service, *cache.Store, *cache.Blobs) {
	t.Helper()
	dir := t.TempDir()
	store, err := cache.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	blobs, err := cache.NewBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(discard{}, nil))
	reg := metrics.New()
	svc := NewService(store, blobs, ServiceConfig{
		Workers:           1,
		QueueSize:         8,
		MinInterval:       0,
		InferenceTimeout:  2 * time.Second,
		MaxAttempts:       2,
		ThresholdNSFW:     0.5,
		ThresholdNSFL:     0.5,
		GrayZoneThreshold: 0.6,
		GrayZoneAction:    "reject",
		TTLApproved:       time.Hour,
		TTLRejected:       time.Hour,
	}, log, reg, nil)
	return svc, store, blobs
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func seedPending(t *testing.T, store *cache.Store, blobs *cache.Blobs, hash string, c color.RGBA) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.InsertPendingFetch(ctx, hash, cache.AlgMD5, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	data := seedPNG(c)
	rel, err := blobs.Write(hash, "png", data)
	if err != nil {
		t.Fatal(err)
	}
	err = store.InsertPendingReview(ctx, &cache.Entry{
		Hash: hash, HashAlg: cache.AlgMD5, ContentType: "image/png",
		BlobPath: rel, Width: 1, Height: 1, Bytes: int64(len(data)),
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

func TestWorkerApprovesAndRejects(t *testing.T) {
	svc, store, blobs := newTestService(t)
	seedPending(t, store, blobs, hashOK, color.RGBA{R: 255, A: 255})   // red -> SFW
	seedPending(t, store, blobs, hashNSFW, color.RGBA{B: 255, A: 255}) // blue -> NSFW

	svc.SetModerator(&fakeMod{})
	svc.Start()
	svc.Enqueue(hashOK)
	svc.Enqueue(hashNSFW)

	ctx := context.Background()
	waitFor(t, 2*time.Second, func() bool {
		e1, err1 := store.Get(ctx, hashOK)
		e2, err2 := store.Get(ctx, hashNSFW)
		return err1 == nil && err2 == nil &&
			e1.Status == cache.StatusApproved && e2.Status == cache.StatusRejected
	})
	svc.Stop()

	e1, _ := store.Get(ctx, hashOK)
	if e1.ModelVer != "fake@1" {
		t.Errorf("model_ver = %q", e1.ModelVer)
	}
	if e1.GrayZone || e1.ManualOverride {
		t.Error("clean approval must not be gray/manual")
	}
	if e1.VerdictScores == "" {
		t.Error("verdict scores must be persisted")
	}
}

func TestManualOverrideWins(t *testing.T) {
	svc, store, blobs := newTestService(t)
	seedPending(t, store, blobs, hashOK, color.RGBA{R: 255, A: 255})

	ctx := context.Background()
	// Human approved first, then a re-review arrives with an unsafe verdict.
	if err := store.ManualSetStatus(ctx, hashOK, true, false, time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequeueForReview(ctx, []string{hashOK}, true); err != nil {
		t.Fatal(err)
	}
	svc.SetModerator(&fakeMod{})
	svc.process(ctx, hashOK)

	e, err := store.Get(ctx, hashOK)
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != cache.StatusApproved || !e.ManualOverride {
		t.Fatalf("manual override must survive re-review: %+v", e)
	}
	svc.Stop()
}

func TestEngineFailureLeavesPending(t *testing.T) {
	svc, store, blobs := newTestService(t)
	seedPending(t, store, blobs, hashOK, color.RGBA{R: 255, A: 255})
	svc.SetModerator(&fakeMod{deny: true})
	svc.Start()
	svc.Enqueue(hashOK)

	time.Sleep(500 * time.Millisecond)
	e, err := store.Get(context.Background(), hashOK)
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != cache.StatusPendingReview {
		t.Fatalf("failed engine must leave entry pending_review, got %s", e.Status)
	}
	svc.Stop()
}

func TestModeratorReadyAndStatus(t *testing.T) {
	svc, _, _ := newTestService(t)
	if svc.ModeratorReady() {
		t.Fatal("no moderator installed yet")
	}
	svc.SetModerator(&fakeMod{})
	if !svc.ModeratorReady() || svc.EngineStatus() != "fake@1" {
		t.Fatalf("status = %q", svc.EngineStatus())
	}
}
