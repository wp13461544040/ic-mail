package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"icloud-api/internal/domain"
)

func TestFetchActiveIncrementalRecoversTwoHundredWithoutTailTruncation(t *testing.T) {
	for _, esearch := range []bool{false, true} {
		for _, stale := range []bool{false, true} {
			t.Run(fmt.Sprintf("esearch=%v/stale=%v", esearch, stale), func(t *testing.T) {
				caps := imap.CapSet{imap.CapIMAP4rev1: {}}
				if esearch {
					caps[imap.CapESearch] = struct{}{}
				}
				fixture := startArchiveIMAPFixture(t, 0, 0, caps)
				fixture.batchBodies = true
				now := time.Date(2026, 9, 15, 0, 10, 0, 0, time.UTC)
				cutoff := now.Add(-15 * time.Minute)
				uid := uint32(0)
				appendMail := func(address string, at time.Time) {
					t.Helper()
					uid++
					raw := fmt.Sprintf("From: sender@example.test\r\nTo: %s\r\nX-Original-To: %s\r\nMessage-ID: <%d@example.test>\r\nSubject: Verification code\r\n\r\nVerification code %06d\r\n", address, address, uid, 100000+uid)
					fixture.raw[uid] = raw
					if _, err := fixture.user.Append("INBOX", strings.NewReader(raw), &imap.AppendOptions{Time: at}); err != nil {
						t.Fatal(err)
					}
				}
				for i := 0; i < 1000; i++ {
					appendMail(fixture.alias.Address, now.Add(-72*time.Hour))
				}
				wantUIDs := make(map[uint32]bool)
				for i := 0; i < 200; i++ {
					if i == 80 {
						appendMail(fixture.alias.Address, cutoff.Add(-time.Second))
					}
					if i == 50 {
						appendMail(fixture.alias.Address, cutoff.Add(-time.Second))
						appendMail("unknown+tag@example.test", now)
						appendMail("alias+tag@other.test", now)
					}
					address := fixture.alias.Address
					if i%2 == 0 {
						address = "alias+task@example.test"
					}
					// The first message is exactly at cutoff with a previous-day
					// INTERNALDATE in a negative timezone.
					at := cutoff.Add(time.Duration(i) * time.Second).In(time.FixedZone("negative", -10*3600))
					appendMail(address, at)
					wantUIDs[uid] = true
				}
				fetcher := NewFetcher()
				fetcher.ArchiveTempDir = t.TempDir()
				fetcher.MaxIncrementalCandidates = 128
				if stale {
					fetcher.MaxIncrementalCandidates = 64
				}
				fetcher.now = func() time.Time { return now }
				var state *domain.IMAPSyncState
				if stale {
					previous := fixture.initialState(t)
					previous.LastUID, previous.UpdatedAt = 1, now.Add(-time.Hour)
					state = &previous
				}
				count := 0
				for page := 0; page < 4; page++ {
					fetch := fetcher.FetchActiveIncremental
					if stale && page == 1 {
						// A manual ordinary sync between active batches must not
						// discard their durable time boundary.
						fetch = fetcher.FetchIncremental
					}
					result, err := fetch(context.Background(), fixture.account, "fixture-password", []domain.Alias{fixture.alias}, state, nil)
					if err != nil {
						t.Fatal(err)
					}
					count += len(result.ArchivedMessages)
					copy := result.State
					state = &copy
					if !result.HasMore {
						break
					}
					if !state.ActiveWindowSince.Equal(cutoff) {
						t.Fatal("pending recovery cutoff was not pinned")
					}
					now = now.Add(16 * time.Minute)
				}
				if count != 200 || state.LastUID != uid || !state.ActiveWindowSince.IsZero() {
					t.Fatalf("recovery messages=%d cursor=%d/%d window=%v", count, state.LastUID, uid, state.ActiveWindowSince)
				}
				fixture.mu.Lock()
				defer fixture.mu.Unlock()
				if len(fixture.bodyUIDs) != 200 {
					t.Fatalf("body reads=%d, want 200", len(fixture.bodyUIDs))
				}
				for _, uid := range fixture.bodyUIDs {
					if !wantUIDs[uint32(uid)] {
						t.Fatalf("old or unrelated UID body fetched: %d", uid)
					}
					delete(wantUIDs, uint32(uid))
				}
				if len(wantUIDs) != 0 {
					t.Fatalf("missing bodies=%d", len(wantUIDs))
				}
			})
		}
	}
}

func TestFetchActiveIncrementalRecoverySearchFailureKeepsCursor(t *testing.T) {
	f := startArchiveIMAPFixture(t, 200, 0)
	f.searchPolicy = func(criteria *imap.SearchCriteria) error {
		if !criteria.Since.IsZero() {
			return errors.New("fixture date search failed")
		}
		return nil
	}
	fetcher := NewFetcher()
	fetcher.ArchiveTempDir = t.TempDir()
	now := time.Date(2026, 9, 11, 1, 10, 0, 0, time.UTC)
	fetcher.now = func() time.Time { return now }
	state := f.initialState(t)
	state.LastUID, state.UpdatedAt = 1, now.Add(-time.Hour)
	result, err := fetcher.FetchActiveIncremental(context.Background(), f.account, "fixture-password", []domain.Alias{f.alias}, &state, nil)
	if err == nil || result.State != state {
		t.Fatalf("failed search advanced state: %v %v", result.State, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodyUIDs) != 0 {
		t.Fatal("failed date search fetched message bodies")
	}
}

func TestFetchActiveIncrementalStaleObservationCatchup(t *testing.T) {
	t.Run("caught up cursor does not redownload", func(t *testing.T) {
		fixture := startArchiveIMAPFixture(t, 75, 0)
		fetcher := NewFetcher()
		fetcher.ArchiveTempDir = t.TempDir()
		now := time.Date(2026, 9, 11, 1, 16, 0, 0, time.UTC)
		fetcher.now = func() time.Time { return now }
		state := fixture.initialState(t)
		state.LastUID = 75
		state.UpdatedAt = now.Add(-16 * time.Minute)
		result, err := fetcher.FetchActiveIncremental(context.Background(), fixture.account, "fixture-password", []domain.Alias{fixture.alias}, &state, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.State.LastUID != 75 || result.HasMore || len(result.ArchivedMessages) != 0 {
			t.Fatalf("caught-up stale result cursor=%d more=%v archived=%d", result.State.LastUID, result.HasMore, len(result.ArchivedMessages))
		}
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		if len(fixture.bodyUIDs) != 0 {
			t.Fatalf("caught-up stale result fetched bodies=%d, want 0", len(fixture.bodyUIDs))
		}
	})

	t.Run("empty mailbox", func(t *testing.T) {
		fixture := startArchiveIMAPFixture(t, 0, 0)
		fetcher := NewFetcher()
		fetcher.ArchiveTempDir = t.TempDir()
		now := time.Date(2026, 9, 11, 1, 16, 0, 0, time.UTC)
		fetcher.now = func() time.Time { return now }
		state := fixture.initialState(t)
		state.UpdatedAt = now.Add(-16 * time.Minute)
		result, err := fetcher.FetchActiveIncremental(context.Background(), fixture.account, "fixture-password", []domain.Alias{fixture.alias}, &state, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.State.LastUID != 0 || result.HasMore || len(result.ArchivedMessages) != 0 {
			t.Fatalf("empty stale result cursor=%d more=%v archived=%d", result.State.LastUID, result.HasMore, len(result.ArchivedMessages))
		}
	})

	t.Run("new mail is archived", func(t *testing.T) {
		fixture := startArchiveIMAPFixture(t, 75, 0)
		newRaw := "From: sender@example.test\r\nX-Original-To: alias@example.test\r\nTo: alias@example.test\r\nSubject: message 76\r\nMessage-ID: <76@example.test>\r\nDate: Fri, 11 Sep 2026 01:00:00 +0000\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nVerification code 100076\r\n"
		if _, err := fixture.user.Append("INBOX", strings.NewReader(newRaw), &imap.AppendOptions{Time: time.Date(2026, 9, 11, 1, 0, 76, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
		fetcher := NewFetcher()
		fetcher.ArchiveTempDir = t.TempDir()
		now := time.Date(2026, 9, 11, 1, 10, 0, 0, time.UTC)
		fetcher.now = func() time.Time { return now }
		state := fixture.initialState(t)
		state.LastUID = 75
		state.UpdatedAt = now.Add(-16 * time.Minute)
		result, err := fetcher.FetchActiveIncremental(context.Background(), fixture.account, "fixture-password", []domain.Alias{fixture.alias}, &state, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.State.LastUID != 76 || result.HasMore || len(result.ArchivedMessages) != 1 {
			t.Fatalf("new-mail stale result cursor=%d more=%v archived=%d", result.State.LastUID, result.HasMore, len(result.ArchivedMessages))
		}
	})
}

// These tests use the TLS IMAP fixture from archive_fetcher_transport_test.go;
// in particular they exercise the same LOGIN/SELECT/FETCH path as production.
func TestFetchActiveIncrementalInitialTailAndStaleCursor(t *testing.T) {
	fixture := startArchiveIMAPFixture(t, 130, 0)
	fetcher := NewFetcher()
	fetcher.ArchiveTempDir = t.TempDir()
	fetcher.ActiveRecentCandidates = 128
	fetcher.MaxIncrementalCandidates = 128
	fetcher.now = func() time.Time { return time.Date(2026, 9, 11, 1, 0, 0, 0, time.UTC) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := fetcher.FetchActiveIncremental(ctx, fixture.account, "fixture-password", []domain.Alias{fixture.alias}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reset || len(result.ArchivedMessages) != 128 {
		t.Fatalf("active initial result reset=%v archived=%d, want reset and 128", result.Reset, len(result.ArchivedMessages))
	}
	if !result.HasMore || result.State.ActiveWindowSince.IsZero() {
		t.Fatal("initial page did not retain its pending time window")
	}
	state := result.State
	result, err = fetcher.FetchActiveIncremental(ctx, fixture.account, "fixture-password", []domain.Alias{fixture.alias}, &state, nil)
	if err != nil || len(result.ArchivedMessages) != 2 || result.HasMore || !result.State.ActiveWindowSince.IsZero() {
		t.Fatalf("initial remainder: messages=%d more=%v err=%v", len(result.ArchivedMessages), result.HasMore, err)
	}
	// Replaying the fully committed cursor must not download another tail.
	state = result.State
	fixture.mu.Lock()
	fixture.bodyUIDs = nil
	fixture.mu.Unlock()
	result, err = fetcher.FetchActiveIncremental(ctx, fixture.account, "fixture-password", []domain.Alias{fixture.alias}, &state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ArchivedMessages) != 0 || result.State.LastUID != state.LastUID {
		t.Fatalf("active same-cursor result archived=%d cursor=%d, want no-op at %d", len(result.ArchivedMessages), result.State.LastUID, state.LastUID)
	}
	fixture.mu.Lock()
	if len(fixture.bodyUIDs) != 0 {
		t.Fatalf("same active cursor fetched bodies=%d, want 0", len(fixture.bodyUIDs))
	}
	fixture.mu.Unlock()
	state = result.State
	state.LastUID = 1 // stale cursor: active fetch must still use the recent tail.
	state.UIDValidity++
	result, err = fetcher.FetchActiveIncremental(ctx, fixture.account, "fixture-password", []domain.Alias{fixture.alias}, &state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reset || len(result.ArchivedMessages) != 128 {
		t.Fatalf("active stale/reset result reset=%v archived=%d, want reset and 128", result.Reset, len(result.ArchivedMessages))
	}
}

func TestFetchActiveIncrementalSkipsOldBodies(t *testing.T) {
	fixture := startArchiveIMAPFixture(t, 2, 0)
	fetcher := NewFetcher()
	fetcher.ArchiveTempDir = t.TempDir()
	fetcher.now = func() time.Time { return time.Date(2026, 9, 11, 1, 16, 0, 0, time.UTC) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := fetcher.FetchActiveIncremental(ctx, fixture.account, "fixture-password", []domain.Alias{fixture.alias}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ArchivedMessages) != 0 {
		t.Fatalf("old active messages archived=%d, want 0", len(result.ArchivedMessages))
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.bodyUIDs) != 0 {
		t.Fatalf("old active messages fetched bodies=%d, want 0", len(fixture.bodyUIDs))
	}
}

func TestFetchActiveIncrementalLiveBacklogContinuesWithoutSkipping(t *testing.T) {
	f := startArchiveIMAPFixture(t, 140, 0)
	fetcher := NewFetcher()
	fetcher.ArchiveTempDir = t.TempDir()
	fetcher.MaxIncrementalCandidates = 32
	now := time.Date(2026, 9, 11, 1, 4, 0, 0, time.UTC)
	fetcher.now = func() time.Time { return now }
	state := f.initialState(t)
	state.UpdatedAt = now
	count := 0
	for i := 0; i < 5; i++ {
		result, err := fetcher.FetchActiveIncremental(context.Background(), f.account, "fixture-password", []domain.Alias{f.alias}, &state, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.State.LastUID <= state.LastUID {
			t.Fatal("live cursor did not advance")
		}
		if result.RecoveryBoundaryUID != 0 {
			t.Fatal("live backlog unexpectedly truncated")
		}
		count += len(result.ArchivedMessages)
		state = result.State
		if !result.HasMore {
			break
		}
	}
	if count != 140 || state.LastUID != 140 {
		t.Fatalf("live backlog messages=%d cursor=%d", count, state.LastUID)
	}
	// A stale cursor in the same generation retains all recent messages, not
	// just the last 128.
	state.LastUID = 1
	state.UpdatedAt = now.Add(-time.Hour)
	fetcher.MaxIncrementalCandidates = 128
	result, err := fetcher.FetchActiveIncremental(context.Background(), f.account, "fixture-password", []domain.Alias{f.alias}, &state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reset || result.RecoveryBoundaryUID != 1 || len(result.ArchivedMessages) != 128 || !result.HasMore {
		t.Fatalf("stale recovery: reset=%v boundary=%d messages=%d", result.Reset, result.RecoveryBoundaryUID, len(result.ArchivedMessages))
	}
}
