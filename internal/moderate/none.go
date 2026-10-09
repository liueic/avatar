package moderate

import (
	"context"
	"fmt"
	"image"
)

// NonePolicy decides what engine "none" does with every image.
type NonePolicy string

const (
	NoneApprove NonePolicy = "approve"
	NoneReject  NonePolicy = "reject"
)

// None is the trivial Moderator used for debugging and for operating with
// moderation disabled (SPEC §9.1: engine=none). It approves or rejects every
// image according to its policy.
type None struct {
	Policy NonePolicy
}

// NewNone builds the none engine.
func NewNone(policy NonePolicy) *None {
	if policy != NoneApprove && policy != NoneReject {
		policy = NoneReject
	}
	return &None{Policy: policy}
}

// Classify implements Moderator.
func (n *None) Classify(ctx context.Context, img image.Image) (Result, error) {
	if img == nil {
		return Result{}, fmt.Errorf("none: nil image")
	}
	switch n.Policy {
	case NoneApprove:
		return Result{
			Verdict:  VerdictSFW,
			Scores:   map[string]float32{"SFW": 1, "NSFW": 0, "NSFL": 0},
			ModelVer: "none/approve@1",
		}, nil
	default:
		return Result{
			Verdict:  VerdictNSFW,
			Scores:   map[string]float32{"SFW": 0, "NSFW": 1, "NSFL": 0},
			ModelVer: "none/reject@1",
		}, nil
	}
}

// Close implements Moderator.
func (n *None) Close() error { return nil }

// ModelVersion reports the engine identity recorded on reviewed entries.
func (n *None) ModelVersion() string {
	return string(n.Policy)
}
