package onnx

import (
	"image"
	"image/color"
	"testing"
)

func TestRealModelSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	m, err := New(Config{
		ModelPath:   "../../../models/image-safety-classifier-xs.onnx",
		ORTLibPath:  "../../../third_party/onnxruntime/lib/libonnxruntime.dylib",
		ModelSHA256: "8c28c49d9075f3ad15ebdc2961f02d5b3f99be944815b848b49c9f0e6f3fb689",
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer m.Close()

	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	res, err := m.Classify(t.Context(), img)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	t.Logf("verdict=%v scores=%v model=%s dur=%s", res.Verdict, res.Scores, res.ModelVer, res.Duration)
	if res.Scores["SFW"]+res.Scores["NSFW"]+res.Scores["NSFL"] < 0.99 {
		t.Fatalf("softmax scores do not sum to 1: %v", res.Scores)
	}
}
