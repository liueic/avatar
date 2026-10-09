// Package cache implements the entry store (SQLite metadata + filesystem
// blobs) and the entry state machine described in SPEC §5 and §10.
package cache

import (
	"errors"
	"fmt"
	"strings"
)

// Status is the state-machine status of an entry (SPEC §5).
type Status string

const (
	StatusPendingFetch  Status = "pending_fetch"
	StatusPendingReview Status = "pending_review"
	StatusApproved      Status = "approved"
	StatusRejected      Status = "rejected"
	StatusRejectedFetch Status = "rejected_fetch"
)

// Valid reports whether s is one of the known statuses.
func (s Status) Valid() bool {
	switch s {
	case StatusPendingFetch, StatusPendingReview, StatusApproved, StatusRejected, StatusRejectedFetch:
		return true
	}
	return false
}

// AllStatuses lists every known status in stable order.
func AllStatuses() []Status {
	return []Status{StatusPendingFetch, StatusPendingReview, StatusApproved, StatusRejected, StatusRejectedFetch}
}

// HashAlg identifies the hash algorithm inferred from the hash length.
type HashAlg string

const (
	AlgMD5    HashAlg = "md5"
	AlgSHA256 HashAlg = "sha256"
)

// AlgForHash infers the algorithm from a validated hash.
func AlgForHash(hash string) HashAlg {
	if len(hash) == 32 {
		return AlgMD5
	}
	return AlgSHA256
}

// Entry is one cache record: a hash, its image blob and its review state.
type Entry struct {
	Hash           string
	HashAlg        HashAlg
	Status         Status
	ContentType    string
	BlobPath       string
	Width          int
	Height         int
	Bytes          int64
	Rev            int64 // bumped on every change of served content; feeds the ETag
	VerdictScores  string
	ModelVer       string
	ManualOverride bool
	GrayZone       bool
	FailReason     string
	FailCount      int
	FetchedAt      int64
	ReviewedAt     int64
	ExpiresAt      int64
	LastAccessed   int64
}

// ManualOverrideError is returned when an operation would overwrite a manual
// review decision without force=true (SPEC §12).
var ManualOverrideError = errors.New("entry has a manual review override")

// ValidateHash enforces the Gravatar hash format: 32 (MD5) or 64 (SHA-256)
// lowercase hex characters (SPEC §4.1).
func ValidateHash(h string) bool {
	if len(h) != 32 && len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// BlobExt maps a normalized content type to a blob file extension.
func BlobExt(contentType string) (string, error) {
	switch contentType {
	case "image/jpeg":
		return "jpg", nil
	case "image/png":
		return "png", nil
	default:
		return "", fmt.Errorf("unsupported blob content type %q", contentType)
	}
}

// ParseStatus converts a raw string to a Status, returning an error for
// unknown values.
func ParseStatus(s string) (Status, error) {
	st := Status(strings.ToLower(strings.TrimSpace(s)))
	if !st.Valid() {
		return "", fmt.Errorf("unknown status %q", s)
	}
	return st, nil
}
