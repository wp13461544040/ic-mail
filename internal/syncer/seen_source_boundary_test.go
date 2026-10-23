package syncer

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"icloud-api/internal/domain"
)

type seenSourceLocker struct {
	events []string
	denied bool
}

func (l *seenSourceLocker) WithAccountLock(_ context.Context, _ int64, operation func() error) error {
	l.events = append(l.events, "management")
	if l.denied {
		return context.DeadlineExceeded
	}
	return operation()
}

func (l *seenSourceLocker) WithAccountIMAPSlot(_ context.Context, _ int64, operation func() error) error {
	l.events = append(l.events, "imap")
	return operation()
}

func TestSeenWorkerPinsSourceBeforeIMAPSlot(t *testing.T) {
	for _, denied := range []bool{false, true} {
		repo := &seenWorkerRepo{accounts: map[int64]domain.Account{1: {ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}}}
		marker := &seenMarkerStub{}
		locker := &seenSourceLocker{denied: denied}
		worker := NewSeenWorker(repo, cipherFunc(fixedCipher), marker, locker, discardLogger(), time.Minute)
		work, err := groupSeenTasks(1, []domain.SeenTask{{AccountID: 1, UIDValidity: 91, UID: 10}})
		if err != nil {
			t.Fatal(err)
		}
		err = worker.processAccount(context.Background(), work)
		if denied {
			if !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(locker.events, []string{"management"}) || len(marker.snapshot()) != 0 {
				t.Fatal("denied management source acquired IMAP resources")
			}
		} else if err != nil || !reflect.DeepEqual(locker.events, []string{"management", "imap"}) || len(marker.snapshot()) != 1 {
			t.Fatalf("seen source lock ordering failed: %v", err)
		}
	}
}
