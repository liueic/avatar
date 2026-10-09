package cache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

const (
	h1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	h3 = "cccccccccccccccccccccccccccccccc"
)

// TestStateMachine exercises the SPEC §5 transitions through the store API.
func TestStateMachine(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// absent -> pending_fetch
	created, err := s.InsertPendingFetch(ctx, h1, AlgMD5, time.Now().Add(time.Minute).Unix())
	if err != nil || !created {
		t.Fatalf("insert pending_fetch: %v %v", created, err)
	}
	// duplicate insert is not an error and reports created=false
	created, err = s.InsertPendingFetch(ctx, h1, AlgMD5, time.Now().Add(time.Minute).Unix())
	if err != nil || created {
		t.Fatalf("duplicate insert: created=%v err=%v", created, err)
	}

	// pending_fetch -> pending_review (content rev starts at 1)
	err = s.InsertPendingReview(ctx, &Entry{
		Hash: h1, HashAlg: AlgMD5, Status: StatusPendingReview,
		ContentType: "image/png", BlobPath: "aa/h1.png", Width: 1, Height: 1, Bytes: 100,
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := s.Get(ctx, h1)
	if e.Status != StatusPendingReview || e.Rev != 1 {
		t.Fatalf("after review insert: %+v", e)
	}

	// pending_review -> approved; rev bumps (ETag changes)
	if err := s.MarkReviewed(ctx, h1, "m@1", `{"SFW":0.9}`, true, false, false, time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	e, _ = s.Get(ctx, h1)
	if e.Status != StatusApproved || e.Rev != 2 {
		t.Fatalf("after approve: status=%s rev=%d", e.Status, e.Rev)
	}
	if e.ExpiresAt <= time.Now().Unix() {
		t.Fatal("approved entry must carry a future expiry")
	}

	// approved -> pending_review -> rejected (re-review flip)
	if _, err := s.RequeueForReview(ctx, []string{h1}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkReviewed(ctx, h1, "m@1", `{"NSFW":0.9}`, false, false, false, time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	e, _ = s.Get(ctx, h1)
	if e.Status != StatusRejected || e.Rev != 3 {
		t.Fatalf("after reject: status=%s rev=%d", e.Status, e.Rev)
	}
}

func TestManualOverrideProtection(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	s.InsertPendingFetch(ctx, h1, AlgMD5, 0)
	s.InsertPendingReview(ctx, &Entry{Hash: h1, HashAlg: AlgMD5, ContentType: "image/png", BlobPath: "aa/h1.png", Bytes: 1}, time.Hour)

	// Human approves.
	if err := s.ManualSetStatus(ctx, h1, true, false, time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Get(ctx, h1)
	if !e.ManualOverride || e.Status != StatusApproved {
		t.Fatalf("manual approve not recorded: %+v", e)
	}

	// Normal re-review must not touch manual entries at all.
	requeued, _ := s.RequeueForReview(ctx, []string{h1}, true)
	if len(requeued) != 0 {
		t.Fatalf("manual entry requeued without force: %v", requeued)
	}
	e, _ = s.Get(ctx, h1)
	if e.Status != StatusApproved {
		t.Fatalf("manual decision disturbed: %+v", e)
	}

	// TOCTOU belt-and-braces: a row that is somehow pending_review while
	// carrying an override must still refuse an automatic verdict.
	if _, err := s.RequeueForReview(ctx, []string{h1}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkReviewed(ctx, h1, "m@1", `{"SFW":0.1}`, false, false, false, time.Hour, time.Hour); err == nil {
		t.Fatal("MarkReviewed must refuse manual-override entries")
	} else if err != ManualOverrideError {
		t.Fatalf("want ManualOverrideError, got %v", err)
	}
	e, _ = s.Get(ctx, h1)
	if e.Status == StatusRejected {
		t.Fatalf("manual decision overwritten: %+v", e)
	}

	// force=true clears the override.
	requeued, _ = s.ForceRequeue(ctx, []string{h1})
	if len(requeued) != 1 {
		t.Fatalf("force requeue failed: %v", requeued)
	}
	e, _ = s.Get(ctx, h1)
	if e.Status != StatusPendingReview || e.ManualOverride {
		t.Fatalf("force requeue: %+v", e)
	}
}

func TestNegativeTTLAndBackoff(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// not_found negative with long TTL
	if err := s.MarkNegative(ctx, h1, AlgMD5, "not_found", time.Now().Add(24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Get(ctx, h1)
	if e.Status != StatusRejectedFetch || e.FailReason != "not_found" || e.FailCount != 1 {
		t.Fatalf("negative entry: %+v", e)
	}

	// repeated failures bump fail_count (backoff driver, SPEC §6.3)
	s.ClaimNegativeRefetch(ctx, h1, time.Now().Add(time.Minute).Unix())
	if err := s.MarkNegative(ctx, h1, AlgMD5, "network", time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	e, _ = s.Get(ctx, h1)
	if e.FailCount != 2 || e.FailReason != "network" {
		t.Fatalf("fail_count/backoff: %+v", e)
	}

	// unexpired negatives are not claimable
	ok, err := s.ClaimNegativeRefetch(ctx, h1, time.Now().Add(time.Hour).Unix())
	if err != nil || ok {
		t.Fatalf("unexpired negative claimed: %v %v", ok, err)
	}

	// expired negatives are claimable: backdate the TTL, then claim
	if err := s.MarkNegative(ctx, h1, AlgMD5, "network", time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	ok, err = s.ClaimNegativeRefetch(ctx, h1, time.Now().Add(time.Hour).Unix())
	if err != nil || !ok {
		t.Fatalf("expired negative not claimable: %v %v", ok, err)
	}
	e, _ = s.Get(ctx, h1)
	if e.Status != StatusPendingFetch {
		t.Fatalf("claimed negative status = %s", e.Status)
	}
}

func TestLRUEvictionOrder(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	now := time.Now().Unix()
	for i, h := range []string{h1, h2, h3} {
		s.InsertPendingFetch(ctx, h, AlgMD5, now+3600)
		s.InsertPendingReview(ctx, &Entry{
			Hash: h, HashAlg: AlgMD5, ContentType: "image/png",
			BlobPath: h[:2] + "/" + h + ".png", Bytes: int64(100 * (i + 1)),
		}, time.Hour)
		s.MarkReviewed(ctx, h, "m@1", `{"SFW":0.9}`, true, false, false, time.Hour, time.Hour)
	}

	// Touch h1 and h3 recently; h2 is oldest.
	s.TouchAccess(ctx, h1, time.Hour)
	time.Sleep(10 * time.Millisecond)
	s.TouchAccess(ctx, h3, time.Hour)
	time.Sleep(10 * time.Millisecond)
	s.TouchAccess(ctx, h1, time.Hour)

	victims, err := s.LRUVictims(ctx, 150) // need to free 150 bytes: h2 (200) + h1 (100)? order: h2 (oldest) first
	if err != nil {
		t.Fatal(err)
	}
	if len(victims) == 0 || victims[0].Hash != h2 {
		t.Fatalf("first victim = %+v, want h2 (least recently accessed)", victims)
	}
	// h2 must be gone.
	if _, err := s.Get(ctx, h2); err != ErrNotFound {
		t.Fatalf("victim still present: %v", err)
	}
	// Pending entries are never LRU victims.
	s.InsertPendingFetch(ctx, "dddddddddddddddddddddddddddddddd", AlgMD5, now+3600)
	victims, _ = s.LRUVictims(ctx, 1<<30)
	for _, v := range victims {
		if v.Status != StatusApproved {
			t.Fatalf("non-approved victim: %+v", v)
		}
	}
}

func TestExpirySweeps(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour).Unix()

	// approved expired -> delete row (and blob via cleaner)
	s.InsertPendingFetch(ctx, h1, AlgMD5, past)
	s.InsertPendingReview(ctx, &Entry{Hash: h1, HashAlg: AlgMD5, ContentType: "image/png", BlobPath: "aa/h1.png", Bytes: 5}, time.Hour)
	s.MarkReviewed(ctx, h1, "m@1", `{"SFW":1}`, true, false, false, time.Hour, time.Hour)
	s.db.Exec(`UPDATE entries SET expires_at = ? WHERE hash = ?`, past, h1)

	// rejected: blob retention keeps the record (SPEC §10.3)
	s.InsertPendingFetch(ctx, h2, AlgMD5, past)
	s.InsertPendingReview(ctx, &Entry{Hash: h2, HashAlg: AlgMD5, ContentType: "image/png", BlobPath: "bb/h2.png", Bytes: 5}, time.Hour)
	s.MarkReviewed(ctx, h2, "m@1", `{"NSFW":1}`, false, false, false, time.Hour, time.Hour)
	s.db.Exec(`UPDATE entries SET expires_at = ? WHERE hash = ?`, past, h2)

	// expired rejected_fetch -> DeleteExpired removes it entirely
	s.MarkNegative(ctx, h3, AlgMD5, "network", past)

	removed, err := s.DeleteExpired(ctx, time.Now().Unix(), StatusApproved, StatusPendingFetch, StatusRejectedFetch)
	if err != nil {
		t.Fatal(err)
	}
	hashes := map[string]bool{}
	for _, e := range removed {
		hashes[e.Hash] = true
	}
	if !hashes[h1] || !hashes[h3] {
		t.Fatalf("expired sweep removed: %v", hashes)
	}

	kept, err := s.ExpireKeepRecord(ctx, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].Hash != h2 {
		t.Fatalf("keep-record: %v", kept)
	}
	e, _ := s.Get(ctx, h2)
	if e.BlobPath != "" || e.Status != StatusRejected {
		t.Fatalf("keep-record entry: %+v", e)
	}
}

func TestPendingStaleSweep(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	s.InsertPendingFetch(ctx, h1, AlgMD5, time.Now().Add(time.Hour).Unix())
	s.InsertPendingReview(ctx, &Entry{Hash: h1, HashAlg: AlgMD5, ContentType: "image/png", BlobPath: "aa/h1.png", Bytes: 1}, time.Hour)
	// Backdate fetched_at so it looks stale.
	s.db.Exec(`UPDATE entries SET fetched_at = ? WHERE hash = ?`, time.Now().Add(-25*time.Hour).Unix(), h1)

	stale, err := s.PendingOlderThan(ctx, StatusPendingReview, time.Now().Add(-24*time.Hour).Unix(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != h1 {
		t.Fatalf("stale scan: %v", stale)
	}
}

func TestHashValidation(t *testing.T) {
	valid := []string{
		"d6a92cf5e6a7a9e10f2f3e4b5c6d7e8f",                                 // md5
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", // sha256
	}
	for _, h := range valid {
		if !ValidateHash(h) {
			t.Errorf("ValidateHash(%q) = false", h)
		}
	}
	invalid := []string{
		"", "short", strings.Title("d6a92cf5e6a7a9e10f2f3e4b5c6d7e8f"),
		"d6a92cf5e6a7a9e10f2f3e4b5c6d7e8g",                     // non-hex
		"D6A92CF5E6A7A9E10F2F3E4B5C6D7E8F",                     // uppercase
		"0123456789abcdef0123456789abcdef0123456789abcdef0123", // 52 chars
	}
	for _, h := range invalid {
		if ValidateHash(h) {
			t.Errorf("ValidateHash(%q) = true, want false", h)
		}
	}
}

// TestOpenCreatesParentDir guards the fresh-checkout experience: opening a DB
// in a not-yet-existing directory must succeed (SQLITE_CANTOPEN regression).
func TestOpenCreatesParentDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist")
	s, err := Open(filepath.Join(dir, "avater.db"))
	if err != nil {
		t.Fatalf("open in missing dir: %v", err)
	}
	s.Close()
	if fi, err := os.Stat(filepath.Join(dir, "avater.db")); err != nil || fi.IsDir() {
		t.Fatalf("db file not created: %v", err)
	}
}
