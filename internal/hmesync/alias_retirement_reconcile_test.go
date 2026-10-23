package hmesync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

func TestReconcileRetirementAliasesOriginalTargetsAndReadOnly(t *testing.T) {
	client := &fakeAppleClient{}
	locker := newFakeAcquiringLocker()
	service, repo, ids, directory := newAliasDeletionBatchFixture(t, 3, client, locker)
	repo.mu.Lock()
	delete(repo.aliases, ids[0])
	repo.mu.Unlock()
	// One disappeared locally but remains at Apple; one remains locally but
	// disappeared at Apple; one remains deactivated in Apple's directory.
	directory.Aliases = append(directory.Aliases[:1], directory.Aliases[2])
	directory.Aliases[1].IsActive = false
	validates, lists := 0, 0
	client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
		validates++
		if len(locker.token) != 0 {
			t.Error("validation escaped the account lock")
		}
		session.SessionToken = "validated"
		session.Cookies = []apple.PersistentCookie{{Name: "fixture", Value: "validated"}}
		return session, nil
	}
	client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		lists++
		if len(locker.token) != 0 || session.SessionToken != "validated" || len(session.Cookies) != 1 || session.Cookies[0].Value != "validated" {
			t.Error("directory read escaped locking or lost the validation checkpoint")
		}
		assertStoredAppleSessionToken(t, service, repo, 3, "validated")
		session.SessionToken = "listed"
		session.Cookies[0].Value = "listed"
		return directory, session, nil
	}
	repo.getAliasFn = func(context.Context, int64) error { t.Error("read reloaded a local alias"); return nil }
	targets := make([]AliasRetirementTarget, len(ids))
	for index, id := range ids {
		targets[index] = AliasRetirementTarget{AliasID: id, AccountID: 3, Address: fmt.Sprintf("alias%d@icloud.com", id)}
	}
	// A deletion recovery context must not introduce GetAlias or mutations.
	ctx := WithAliasDeletionRecovery(context.Background(), nil)
	results, err := service.ReconcileRetirementAliases(ctx, targets)
	if err != nil || len(results) != 3 || validates != 1 || lists != 1 {
		t.Fatalf("read result: err=%v results=%d validates=%d lists=%d", err, len(results), validates, lists)
	}
	for index, result := range results {
		if result.AliasID != ids[index] || result.Err != nil || result.Present != (index != 1) {
			t.Errorf("observation %d = %+v", index, result)
		}
	}
	if client.deactivateCalls.Load() != 0 || client.deleteCalls.Load() != 0 || repo.aliasDeletes.Load() != 0 || repo.imports.Load() != 0 || !repo.hasAlias(ids[1]) {
		t.Fatal("read changed local or remote aliases")
	}
	assertStoredAppleSessionToken(t, service, repo, 3, "listed")
}

func TestReconcileRetirementAliasesUnknownDirectoryNeverProvesAbsence(t *testing.T) {
	for _, scenario := range []string{"list error", "rate limit", "validate error", "foreign target", "foreign directory", "duplicate address", "duplicate ID", "invalid address", "returned Apple ID", "returned DSID", "checkpoint failure", "account changed"} {
		t.Run(scenario, func(t *testing.T) {
			client := &fakeAppleClient{}
			service, repo, ids, directory := newAliasDeletionBatchFixture(t, 2, client, &fakeLocker{})
			storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal, DSID: "123", SessionToken: "original"})
			validates, lists := 0, 0
			remoteFailure := &apple.Error{Op: "list Hide My Email aliases", Kind: apple.ErrService, StatusCode: 503, ServiceCode: "FIXTURE", Err: errors.New("credential fixture secret response body")}
			client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
				validates++
				if scenario == "validate error" {
					return session, remoteFailure
				}
				if scenario == "returned Apple ID" {
					session.AppleID = "different@example.com"
				}
				if scenario == "returned DSID" {
					session.DSID = "456"
				}
				return session, nil
			}
			client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
				lists++
				session.SessionToken = "failed-read-checkpoint"
				switch scenario {
				case "list error":
					return apple.ListResult{}, session, remoteFailure
				case "rate limit":
					remoteFailure.StatusCode = 429
					remoteFailure.RetryAfter = 2 * time.Hour
					return directory, session, remoteFailure
				case "foreign target":
					directory.Aliases[0].ForwardToEmail = "foreign@icloud.com"
				case "foreign directory":
					directory.SelectedForwardTo = "foreign@icloud.com"
					directory.ForwardToEmails = nil
					for index := range directory.Aliases {
						directory.Aliases[index].ForwardToEmail = "foreign@icloud.com"
					}
				case "duplicate address":
					directory.Aliases[1].HME = directory.Aliases[0].HME
				case "duplicate ID":
					directory.Aliases[1].AnonymousID = directory.Aliases[0].AnonymousID
				case "invalid address":
					directory.Aliases[1].HME = "invalid"
				case "account changed":
					repo.mu.Lock()
					repo.account.Email = "changed@icloud.com"
					repo.mu.Unlock()
				}
				return directory, session, nil
			}
			if scenario == "checkpoint failure" {
				repo.upsertSessionErr = errors.New("checkpoint fixture secret")
			}
			results, err := service.ReconcileRetirementAliases(nil, []AliasRetirementTarget{
				{AliasID: ids[0], AccountID: 3, Address: "alias41@icloud.com"},
				{AliasID: 999, AccountID: 3, Address: "missing@icloud.com"},
			})
			if err != nil || len(results) != 2 || results[0].Err == nil || results[0].Present {
				t.Fatalf("unknown directory classified as absence: results=%+v err=%v", results, err)
			}
			if scenario != "foreign target" && results[1].Err == nil {
				t.Fatal("directory failure proved a missing target absent")
			}
			if scenario == "rate limit" && (!errors.Is(results[0].Err, ErrRateLimited) || apple.RetryDelay(results[0].Err) != 2*time.Hour) {
				t.Fatal("throttle lost its classification or Retry-After")
			}
			if scenario == "list error" || scenario == "validate error" {
				var fingerprint *apple.Error
				if !errors.As(results[0].Err, &fingerprint) || fingerprint.StatusCode != 503 || fingerprint.ServiceCode != "FIXTURE" {
					t.Fatal("read lost upstream diagnostic fields")
				}
			}
			for _, result := range results {
				if result.Err != nil && strings.Contains(result.Err.Error(), "secret") {
					t.Fatal("public diagnostic included an upstream body or credential")
				}
			}
			if validates != 1 || lists > 1 || client.deactivateCalls.Load() != 0 || client.deleteCalls.Load() != 0 || repo.aliasDeletes.Load() != 0 || repo.imports.Load() != 0 {
				t.Fatal("read retried or changed aliases")
			}
			if scenario == "list error" || scenario == "rate limit" {
				assertStoredAppleSessionToken(t, service, repo, 3, "failed-read-checkpoint")
			}
		})
	}
}

func TestReconcileRetirementAliasesAccountEligibilityAndInput(t *testing.T) {
	for _, scenario := range []string{"disabled", "custom mailbox", "invalid target", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			client := &fakeAppleClient{}
			service, repo, ids, _ := newAliasDeletionBatchFixture(t, 1, client, &fakeLocker{})
			target := AliasRetirementTarget{AliasID: ids[0], AccountID: 3, Address: "alias41@icloud.com"}
			ctx := context.Background()
			switch scenario {
			case "disabled":
				repo.account.Enabled = false
			case "custom mailbox":
				repo.account.MailboxType = domain.MailboxTypeCustom
			case "invalid target":
				target.Address = "Display Name <alias41@icloud.com>"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			results, err := service.ReconcileRetirementAliases(ctx, []AliasRetirementTarget{target})
			if err != nil || len(results) != 1 || results[0].Err == nil || results[0].Present {
				t.Fatalf("invalid read = %+v err=%v", results, err)
			}
			if scenario == "disabled" && !errors.Is(results[0].Err, ErrAccountDisabled) {
				t.Fatal("disabled account classification was lost")
			}
		})
	}
}

func TestReconcileRetirementAliasesTwoActiveAccountsAndOrderedResults(t *testing.T) {
	now := time.Now().UTC()
	base := newFakeRepository(domain.Account{}, now)
	repo := &aliasDeletionAccountsRepository{fakeRepository: base, accounts: make(map[int64]domain.Account)}
	locker := &aliasDeletionKeyedLocker{}
	client := &fakeAppleClient{}
	service := newTestService(t, repo, client, locker, func() time.Time { return now })
	var targets []AliasRetirementTarget
	for id := int64(1); id <= 4; id++ {
		account := domain.Account{ID: id, Email: fmt.Sprintf("account%d@icloud.com", id), MailboxType: domain.MailboxTypeICloud, Enabled: true}
		repo.accounts[id] = account
		storeSession(t, service, base, id, apple.Session{AppleID: account.Email, Region: apple.RegionGlobal, SessionToken: "initial"})
		targets = append(targets, AliasRetirementTarget{AliasID: id, AccountID: id, Address: fmt.Sprintf("alias%d@icloud.com", id)})
	}
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var validates, lists atomic.Int32
	client.validate = func(ctx context.Context, session apple.Session) (apple.Session, error) {
		validates.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			return session, nil
		case <-ctx.Done():
			return session, ctx.Err()
		}
	}
	client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		lists.Add(1)
		return apple.ListResult{SelectedForwardTo: session.AppleID, ForwardToEmails: []string{session.AppleID}}, session, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type completion struct {
		results []AliasRetirementObservation
		err     error
	}
	done := make(chan completion, 1)
	go func() {
		results, err := service.ReconcileRetirementAliases(ctx, targets)
		done <- completion{results, err}
	}()
	for index := 0; index < 2; index++ {
		select {
		case <-entered:
		case <-ctx.Done():
			close(release)
			t.Fatal("two accounts did not execute concurrently")
		}
	}
	if validates.Load() != 2 {
		t.Error("active account limit exceeded two")
	}
	close(release)
	result := <-done
	if result.err != nil || len(result.results) != 4 || validates.Load() != 4 || lists.Load() != 4 {
		t.Fatalf("grouped read completion = %+v validates=%d lists=%d", result, validates.Load(), lists.Load())
	}
	for index, observation := range result.results {
		if observation.AliasID != targets[index].AliasID || observation.Present || observation.Err != nil {
			t.Errorf("ordered observation %d = %+v", index, observation)
		}
	}
	locker.mu.Lock()
	defer locker.mu.Unlock()
	if locker.max != 2 || len(locker.held) != 0 {
		t.Fatal("account lock concurrency or release was incorrect")
	}
}

func TestReconcileRetirementAliasesAccountCooldownSurvivesDifferentTargets(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		stage string
		cause error
		delay time.Duration
	}{
		{"validation throttle default", "validate", &apple.Error{Kind: apple.ErrService, StatusCode: 429}, time.Minute},
		{"directory throttle server hint", "list", &apple.Error{Kind: apple.ErrService, StatusCode: 429, RetryAfter: 3 * time.Minute}, 3 * time.Minute},
		{"typed service throttle", "list", &apple.Error{Kind: apple.ErrService, StatusCode: 200, ServiceCode: "-41015"}, time.Minute},
		{"non throttle server hint", "list", &apple.Error{Kind: apple.ErrService, StatusCode: 503, RetryAfter: 45 * time.Second}, 45 * time.Second},
		{"largest joined hint", "list", errors.Join(
			&apple.Error{Kind: apple.ErrService, StatusCode: 503, RetryAfter: 10 * time.Second},
			&apple.Error{Kind: apple.ErrService, StatusCode: 503, RetryAfter: 2 * time.Minute},
		), 2 * time.Minute},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			client := &fakeAppleClient{}
			service, repo, _, _ := newAliasDeletionBatchFixture(t, 1, client, &fakeLocker{})
			now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
			service.now = func() time.Time { return now }
			creationUntil := now.Add(time.Hour)
			service.creationCooldowns = map[int64]map[string]time.Time{3: {"icloud_web": creationUntil}}
			validates, lists := 0, 0
			failed := false
			client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
				validates++
				if scenario.stage == "validate" && !failed {
					failed = true
					return session, scenario.cause
				}
				return session, nil
			}
			client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
				lists++
				if scenario.stage == "list" && !failed {
					failed = true
					return apple.ListResult{}, session, scenario.cause
				}
				return aliasDeletionDirectory(), session, nil
			}
			read := func(aliasID int64) AliasRetirementObservation {
				t.Helper()
				results, err := service.ReconcileRetirementAliases(context.Background(), []AliasRetirementTarget{
					{AliasID: aliasID, AccountID: 3, Address: fmt.Sprintf("original%d@icloud.com", aliasID)},
				})
				if err != nil || len(results) != 1 {
					t.Fatalf("read completion err=%v results=%+v", err, results)
				}
				return results[0]
			}
			if first := read(901); first.Err == nil || first.Present {
				t.Fatal("failed read proved an original target absent")
			}
			beforeValidate, beforeList := validates, lists
			now = now.Add(10 * time.Second)
			for _, aliasID := range []int64{902, 903} {
				cooling := read(aliasID)
				if cooling.Err == nil || Code(cooling.Err) != CodeRateLimited || !errors.Is(cooling.Err, ErrRateLimited) || cooling.Present {
					t.Fatalf("different original target bypassed account cooldown: %+v", cooling)
				}
				var upstream *apple.Error
				if !errors.As(cooling.Err, &upstream) || upstream.StatusCode != 429 || apple.RetryDelay(cooling.Err) != scenario.delay-10*time.Second {
					t.Fatal("cooldown lost its stable fingerprint or remaining server delay")
				}
			}
			if validates != beforeValidate || lists != beforeList {
				t.Fatal("different original targets sent a request during account cooldown")
			}
			now = now.Add(scenario.delay - 10*time.Second)
			if expired := read(904); expired.Err != nil || expired.Present || validates != beforeValidate+1 || lists != beforeList+1 {
				t.Fatalf("expiry did not permit a fresh complete read: %+v validates=%d lists=%d", expired, validates, lists)
			}
			if !service.creationCooldowns[3]["icloud_web"].Equal(creationUntil) {
				t.Fatal("retirement cooldown changed independent creation cooldown")
			}
			if len(service.retirementReadCooldowns) != 0 || client.deleteCalls.Load() != 0 || client.deactivateCalls.Load() != 0 || repo.aliasDeletes.Load() != 0 || repo.imports.Load() != 0 {
				t.Fatal("successful read retained a cooldown or changed aliases")
			}
		})
	}
}

func TestReconcileRetirementAliasesCooldownLeavesOtherAccountsReadable(t *testing.T) {
	client := &fakeAppleClient{}
	service, base, _, _ := newAliasDeletionBatchFixture(t, 1, client, &fakeLocker{})
	other := domain.Account{ID: 4, Email: "other@icloud.com", MailboxType: domain.MailboxTypeICloud, Enabled: true}
	service.repo = &aliasDeletionAccountsRepository{fakeRepository: base, accounts: map[int64]domain.Account{3: base.account, 4: other}}
	storeSession(t, service, base, 4, apple.Session{AppleID: "other@example.com", Region: apple.RegionGlobal, SessionToken: "other"})
	validates, lists := 0, 0
	client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
		validates++
		return session, nil
	}
	client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		lists++
		return apple.ListResult{SelectedForwardTo: other.Email}, session, nil
	}
	service.rememberRetirementReadCooldown(3, &apple.Error{Kind: apple.ErrService, StatusCode: 429, RetryAfter: time.Hour})
	results, err := service.ReconcileRetirementAliases(nil, []AliasRetirementTarget{
		{AliasID: 901, AccountID: 3, Address: "first@icloud.com"},
		{AliasID: 902, AccountID: 4, Address: "second@icloud.com"},
	})
	if err != nil || len(results) != 2 || !errors.Is(results[0].Err, ErrRateLimited) || results[1].Err != nil || results[1].Present || validates != 1 || lists != 1 {
		t.Fatalf("account cooldown leaked across accounts: results=%+v err=%v validate=%d list=%d", results, err, validates, lists)
	}
}
