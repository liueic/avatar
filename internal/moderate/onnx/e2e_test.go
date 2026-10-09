package onnx

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/liueic/avater/internal/cache"
	"github.com/liueic/avater/internal/metrics"
	"github.com/liueic/avater/internal/moderate"
)

// envOr skips the test when the model/library pair is not provisioned.
func envOr(t *testing.T) (model, lib string) {
	t.Helper()
	model = "../../../models/image-safety-classifier-xs.onnx"
	lib = "../../../third_party/onnxruntime/lib/libonnxruntime.dylib"
	if _, err := os.Stat(model); err != nil {
		t.Skip("model not downloaded; run `make model ort`")
	}
	if _, err := os.Stat(lib); err != nil {
		t.Skip("onnxruntime dylib not downloaded; run `make ort`")
	}
	return model, lib
}

// TestServiceEndToEnd wires the real ONNX engine into the real review queue
// and walks one entry through pending_review -> approved (SPEC §18 审核
// 单测/集成测). Skips when the model artifacts are absent.
func TestServiceEndToEnd(t *testing.T) {
	model, lib := envOr(t)

	m, err := New(Config{
		ModelPath:  model,
		ORTLibPath: lib,
	})
	if err != nil {
		t.Fatalf("engine init: %v", err)
	}
	defer m.Close()

	dir := t.TempDir()
	store, err := cache.Open(dir + "/e2e.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	blobs, err := cache.NewBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io_discard{}, nil))
	reg := metrics.New()
	queue := moderate.NewService(store, blobs, moderate.ServiceConfig{
		Workers:           1,
		QueueSize:         4,
		MinInterval:       time.Millisecond,
		InferenceTimeout:  10 * time.Second,
		MaxAttempts:       2,
		ThresholdNSFW:     0.5,
		ThresholdNSFL:     0.5,
		GrayZoneThreshold: 0.6,
		GrayZoneAction:    "reject",
		TTLApproved:       time.Hour,
		TTLRejected:       time.Hour,
	}, log, reg, nil)
	queue.SetModerator(m)
	queue.Start()
	defer queue.Stop()

	// A smooth 64x64 gradient: unambiguously SFW for the classifier.
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 4), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ctx := context.Background()
	if _, err := store.InsertPendingFetch(ctx, hash, cache.AlgMD5, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	rel, err := blobs.Write(hash, "png", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertPendingReview(ctx, &cache.Entry{
		Hash: hash, HashAlg: cache.AlgMD5, ContentType: "image/png",
		BlobPath: rel, Width: 64, Height: 64, Bytes: int64(buf.Len()),
	}, time.Hour); err != nil {
		t.Fatal(err)
	}

	queue.Enqueue(hash)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		e, err := store.Get(ctx, hash)
		if err == nil && e.Status == cache.StatusApproved {
			t.Logf("approved with scores %s (model %s)", e.VerdictScores, e.ModelVer)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e, _ := store.Get(ctx, hash)
	t.Fatalf("entry did not reach approved within timeout: status=%s scores=%s", e.Status, e.VerdictScores)
}

type io_discard struct{}

func (io_discard) Write(p []byte) (int, error) { return len(p), nil }
