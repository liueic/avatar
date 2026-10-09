// Package onnx implements the Moderator interface on top of ONNX Runtime via
// github.com/yalue/onnxruntime_go (SPEC §9.2), targeting the
// OwenElliott/image-safety-classifier-xs model whose preprocessing is baked
// into the graph: resize to 224×224 RGB with 0–255 pixel values, input tensor
// "image" [1,3,224,224], output is post-softmax with class order
// ["NSFL","NSFW","SFW"].
package onnx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yalue/onnxruntime_go"
	xdraw "golang.org/x/image/draw"

	"github.com/liueic/avater/internal/moderate"
)

// InputSize is fixed by the model graph.
const InputSize = 224

// classOrder maps output tensor indices to canonical score keys (SPEC §9.2).
var classOrder = []string{"NSFL", "NSFW", "SFW"}

// Config for the ONNX moderator.
type Config struct {
	ModelPath   string
	ModelSHA256 string // optional integrity check (hex, SPEC §17)
	ORTLibPath  string // shared library to dlopen
	ModelVer    string // recorded on reviewed entries; derived from the digest when empty
}

// Moderator is a process-wide ONNX session. Run calls are serialized to
// respect the CPU budget (SPEC §14: intra-op threads = 1, workers <= 2).
type Moderator struct {
	sess     *onnxruntime_go.DynamicAdvancedSession
	input    string
	output   string
	modelVer string

	mu        sync.Mutex
	closeOnce sync.Once
}

// New verifies the model digest, initializes the ONNX Runtime environment,
// and creates the inference session. The input/output tensor names are
// discovered from the model rather than hardcoded.
func New(cfg Config) (*Moderator, error) {
	if cfg.ModelPath == "" {
		return nil, fmt.Errorf("onnx: empty model path")
	}
	if cfg.ORTLibPath == "" {
		return nil, fmt.Errorf("onnx: empty onnxruntime library path")
	}

	sum, err := fileSHA256(cfg.ModelPath)
	if err != nil {
		return nil, fmt.Errorf("onnx: read model: %w", err)
	}
	if cfg.ModelSHA256 != "" {
		want, verr := normalizeHex(cfg.ModelSHA256)
		if verr != nil {
			return nil, fmt.Errorf("onnx: bad model_sha256: %w", verr)
		}
		if want != hex.EncodeToString(sum) {
			return nil, fmt.Errorf("onnx: model sha256 mismatch: want %s, got %s",
				want, hex.EncodeToString(sum))
		}
	}
	modelVer := cfg.ModelVer
	if modelVer == "" {
		modelVer = "onnx/image-safety-classifier-xs@" + hex.EncodeToString(sum[:6])
	}

	if !onnxruntime_go.IsInitialized() {
		onnxruntime_go.SetSharedLibraryPath(cfg.ORTLibPath)
		if err := onnxruntime_go.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("onnx: initialize onnxruntime (library %s): %w", cfg.ORTLibPath, err)
		}
	}

	inputs, outputs, err := onnxruntime_go.GetInputOutputInfo(cfg.ModelPath)
	if err != nil {
		return nil, fmt.Errorf("onnx: inspect model: %w", err)
	}
	if len(inputs) != 1 || len(outputs) != 1 {
		return nil, fmt.Errorf("onnx: expected 1 input / 1 output, got %d / %d", len(inputs), len(outputs))
	}

	opts, err := onnxruntime_go.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("onnx: session options: %w", err)
	}
	// SPEC §14: single-threaded inference so it cannot starve HTTP goroutines
	// on a 2 vCPU host.
	if err := opts.SetIntraOpNumThreads(1); err != nil {
		return nil, fmt.Errorf("onnx: set intra-op threads: %w", err)
	}

	sess, err := onnxruntime_go.NewDynamicAdvancedSession(
		cfg.ModelPath, []string{inputs[0].Name}, []string{outputs[0].Name}, opts)
	if err != nil {
		return nil, fmt.Errorf("onnx: create session: %w", err)
	}
	return &Moderator{
		sess:     sess,
		input:    inputs[0].Name,
		output:   outputs[0].Name,
		modelVer: modelVer,
	}, nil
}

// Classify implements moderate.Moderator.
func (m *Moderator) Classify(ctx context.Context, img image.Image) (moderate.Result, error) {
	if err := ctx.Err(); err != nil {
		return moderate.Result{}, err
	}
	if img == nil {
		return moderate.Result{}, fmt.Errorf("onnx: nil image")
	}
	start := time.Now()

	data, err := preprocess(img)
	if err != nil {
		return moderate.Result{}, err
	}
	in, err := onnxruntime_go.NewTensor(
		onnxruntime_go.NewShape(1, 3, InputSize, InputSize), data)
	if err != nil {
		return moderate.Result{}, fmt.Errorf("onnx: input tensor: %w", err)
	}
	defer in.Destroy()

	m.mu.Lock()
	outputs := []onnxruntime_go.Value{nil} // nil => runtime allocates the output
	err = m.sess.Run([]onnxruntime_go.Value{in}, outputs)
	m.mu.Unlock()
	if err != nil {
		return moderate.Result{}, fmt.Errorf("onnx: run: %w", err)
	}
	out, ok := outputs[0].(*onnxruntime_go.Tensor[float32])
	if !ok {
		if v := outputs[0]; v != nil {
			_ = v.Destroy()
		}
		return moderate.Result{}, fmt.Errorf("onnx: unexpected output type %T", outputs[0])
	}
	defer out.Destroy()

	scores, verdict, serr := readScores(out)
	if serr != nil {
		return moderate.Result{}, serr
	}
	return moderate.Result{
		Verdict:  verdict,
		Scores:   scores,
		ModelVer: m.modelVer,
		Duration: time.Since(start),
	}, nil
}

// Close implements moderate.Moderator.
func (m *Moderator) Close() error {
	var err error
	m.closeOnce.Do(func() {
		err = m.sess.Destroy()
	})
	return err
}

// ModelVersion reports the engine identity recorded on reviewed entries
// (SPEC §10.2 model_ver).
func (m *Moderator) ModelVersion() string { return m.modelVer }

// preprocess renders img into the NCHW float32 buffer the model expects:
// 224×224, RGB channel order, 0–255 values (normalization is baked into the
// graph — SPEC §9.2).
func preprocess(img image.Image) ([]float32, error) {
	b := img.Bounds()
	if b.Empty() {
		return nil, fmt.Errorf("onnx: empty image")
	}
	resized := image.NewRGBA(image.Rect(0, 0, InputSize, InputSize))
	xdraw.CatmullRom.Scale(resized, resized.Bounds(), img, b, xdraw.Over, nil)

	out := make([]float32, 3*InputSize*InputSize)
	plane := InputSize * InputSize
	for y := 0; y < InputSize; y++ {
		for x := 0; x < InputSize; x++ {
			off := resized.PixOffset(x, y)
			i := y*InputSize + x
			out[i] = float32(resized.Pix[off+0])         // R
			out[plane+i] = float32(resized.Pix[off+1])   // G
			out[2*plane+i] = float32(resized.Pix[off+2]) // B
		}
	}
	return out, nil
}

// readScores converts the post-softmax output into canonical scores and the
// argmax verdict.
func readScores(out *onnxruntime_go.Tensor[float32]) (map[string]float32, moderate.Verdict, error) {
	shape := out.GetShape()
	if len(shape) != 2 || shape[0] != 1 || shape[1] != int64(len(classOrder)) {
		return nil, 0, fmt.Errorf("onnx: unexpected output shape %v", shape)
	}
	data := out.GetData()
	if len(data) < len(classOrder) {
		return nil, 0, fmt.Errorf("onnx: short output data")
	}
	scores := make(map[string]float32, len(classOrder))
	verdict := moderate.VerdictError
	top := float32(math.NaN())
	for i, key := range classOrder {
		v := data[i]
		scores[key] = v
		if math.IsNaN(float64(top)) || v > top {
			top = v
			switch key {
			case "SFW":
				verdict = moderate.VerdictSFW
			case "NSFW":
				verdict = moderate.VerdictNSFW
			case "NSFL":
				verdict = moderate.VerdictNSFL
			}
		}
	}
	return scores, verdict, nil
}

func fileSHA256(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func normalizeHex(s string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if _, err := hex.DecodeString(t); err != nil {
		return "", err
	}
	return t, nil
}

// Version reports the linked onnxruntime library version (admin health,
// SPEC §12).
func Version() string { return onnxruntime_go.GetVersion() }
