// Package moderate defines the Moderator abstraction (SPEC §9.1), the verdict
// decision policy, and the bounded in-process review queue with persistence
// fallback (SPEC §9.3).
package moderate

import (
	"context"
	"image"
	"time"
)

// Verdict is the content-safety classification of one image.
type Verdict int

const (
	VerdictError Verdict = iota // moderation unavailable / inference failed
	VerdictSFW
	VerdictNSFW
	VerdictNSFL
)

// String implements fmt.Stringer.
func (v Verdict) String() string {
	switch v {
	case VerdictSFW:
		return "SFW"
	case VerdictNSFW:
		return "NSFW"
	case VerdictNSFL:
		return "NSFL"
	default:
		return "error"
	}
}

// ScoreKeys are the canonical keys used in Result.Scores and the stored
// verdict_scores JSON (SPEC §9.1).
var ScoreKeys = []string{"SFW", "NSFW", "NSFL"}

// Result is one classification outcome.
type Result struct {
	Verdict  Verdict
	Scores   map[string]float32
	ModelVer string
	Duration time.Duration
}

// Moderator classifies images. Implementations must be safe for concurrent
// use up to the configured worker count.
type Moderator interface {
	Classify(ctx context.Context, img image.Image) (Result, error)
	Close() error
}

// Factory builds a Moderator from the moderation config.
type Factory func(cfg any) (Moderator, error)

// Decision applies the SPEC §9.2 threshold policy to a set of class scores.
// It returns the verdict, whether the image fell into the gray zone, and the
// dominant class scores for persistence.
type Decision struct {
	Approved bool
	GrayZone bool
	Verdict  Verdict
}

// Decide applies:
//   - NSFW >= thresholdNSFW or NSFL >= thresholdNSFL  -> rejected
//   - else if max(score) < grayZoneThreshold          -> gray zone, action decides
//   - else                                            -> approved
func Decide(scores map[string]float32, thresholdNSFW, thresholdNSFL, grayZoneThreshold float64, grayZoneAction string) Decision {
	nsfw := scores["NSFW"]
	nsfl := scores["NSFL"]

	verdict := VerdictSFW
	top := scores["SFW"]
	if nsfw > top {
		top = nsfw
		verdict = VerdictNSFW
	}
	if nsfl > top {
		top = nsfl
		verdict = VerdictNSFL
	}

	if float64(nsfw) >= thresholdNSFW {
		return Decision{Approved: false, Verdict: VerdictNSFW}
	}
	if float64(nsfl) >= thresholdNSFL {
		return Decision{Approved: false, Verdict: VerdictNSFL}
	}
	if float64(top) < grayZoneThreshold {
		// Gray zone: default to rejection ("宁严勿宽") and flag for review.
		approve := grayZoneAction == "approve"
		v := verdict
		if !approve && verdict == VerdictSFW {
			v = VerdictNSFW // conservative: record an unsafe verdict on gray rejects
		}
		return Decision{Approved: approve, GrayZone: true, Verdict: v}
	}
	return Decision{Approved: true, Verdict: VerdictSFW}
}
