package mail

import (
	"context"
	"fmt"
	"os"
	"testing"

	"icloud-api/internal/domain"
)

func TestArchiveBatchBodyFetchPreservesMessagesAndRejectsMissingIdentity(t *testing.T) {
	for _, uidLast := range []bool{false, true} {
		t.Run(fmt.Sprintf("uid_last_%t", uidLast), func(t *testing.T) {
			fixture := startArchiveIMAPFixture(t, 32, 0)
			fixture.batchBodies, fixture.bodyUIDLast = true, uidLast
			fetcher := NewFetcher()
			fetcher.ArchiveTempDir = t.TempDir()
			state := fixture.initialState(t)
			result, err := fetcher.FetchIncremental(context.Background(), fixture.account, "fixture-password", []domain.Alias{fixture.alias}, &state, nil)
			if uidLast {
				// The streaming IMAP dependency needs UID before the literal.
				// An unsupported order must fail without moving the cursor.
				files, readErr := os.ReadDir(fetcher.ArchiveTempDir)
				if err == nil || result.State.LastUID != state.LastUID || len(result.ArchivedMessages) != 0 || readErr != nil || len(files) != 0 {
					t.Fatal("unsupported response order did not fail atomically")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(result.ArchivedMessages) != 32 || result.State.LastUID != 32 || result.HasMore {
				t.Fatal("batch did not preserve every UID and cursor")
			}
			fixture.mu.Lock()
			fetches, bodies := fixture.bodyFetches, len(fixture.bodyUIDs)
			fixture.mu.Unlock()
			if fetches != 1 || bodies != 32 {
				t.Fatalf("body commands=%d bodies=%d, want 1 and 32", fetches, bodies)
			}
			for i, message := range result.ArchivedMessages {
				raw, err := os.ReadFile(message.RawMIMEPath)
				if err != nil || string(raw) != fixture.raw[message.UID] || message.UID != uint32(i+1) || message.OTP != fmt.Sprintf("%06d", 100000+i+1) || message.RawSHA256 == "" {
					t.Fatalf("message %d lost content or identity", i)
				}
			}
		})
	}
}
