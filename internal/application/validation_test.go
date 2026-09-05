package application

import (
	"context"
	"errors"
	"testing"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// T131：验证期间 profile 被编辑，旧结果按 revision 条件丢弃；最新 revision 重新验证后才生效。
func TestStaleValidationResultIsDiscarded(t *testing.T) {
	fixture := newFeatureFixture(t)
	input := validProfileInput(t)
	id, err := fixture.profiles.RegisterProfile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	edit := input
	edit.Name = "Renamed during validation"
	edit.ServerKey = ""
	edit.RequestID = appID(t)
	edit.ExpectedRevision = 0
	fixture.adapter.OnValidateProfile = func() {
		if err := fixture.profiles.UpdateProfile(context.Background(), id, edit); err != nil {
			t.Errorf("concurrent edit: %v", err)
		}
	}
	if err := fixture.profiles.RunValidation(context.Background(), id); !errors.Is(err, ErrValidationStale) {
		t.Fatalf("stale validation err = %v", err)
	}
	record, _ := fixture.store.Profile(context.Background(), id)
	if record.Profile.Compatibility != domain.CompatibilityUnverified || record.Profile.Revision != 1 || record.Profile.LastValidatedAt != nil {
		t.Fatalf("stale result applied: %#v", record.Profile)
	}
	var identities int
	if err := fixture.store.DB().Read.QueryRow(`SELECT COUNT(*) FROM xray_user_identities WHERE kind='bootstrap'`).Scan(&identities); err != nil || identities != 0 {
		t.Fatalf("bootstrap identities after stale validation = %d, %v", identities, err)
	}
	events, _, _ := fixture.store.AuditEvents(context.Background(), ports.AuditFilter{Action: domain.ActionProfileValidated, Limit: 10})
	if len(events) != 0 {
		t.Fatalf("stale validation wrote %d audit events", len(events))
	}
	if err := fixture.profiles.RunValidation(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	record, _ = fixture.store.Profile(context.Background(), id)
	if record.Profile.Compatibility != domain.CompatibilityCompatible || record.Profile.Revision != 1 {
		t.Fatalf("fresh validation not applied: %#v", record.Profile)
	}
}

// T131：手动重新验证按版本条件置回 unverified 并递增 revision；同请求重放幂等，过期版本冲突。
func TestRevalidateIsVersionedAndIdempotent(t *testing.T) {
	fixture := newFeatureFixture(t)
	id := registerCompatibleProfile(t, fixture)
	requested := 0
	fixture.profiles.notify = func(domain.ID) { requested++ }
	request := RevalidateInput{ID: id, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}
	if replay, err := fixture.profiles.Revalidate(context.Background(), request); err != nil || replay {
		t.Fatalf("first revalidate replay=%v err=%v", replay, err)
	}
	record, _ := fixture.store.Profile(context.Background(), id)
	if record.Profile.Compatibility != domain.CompatibilityUnverified || record.Profile.Revision != 1 {
		t.Fatalf("after revalidate: %#v", record.Profile)
	}
	if replay, err := fixture.profiles.Revalidate(context.Background(), request); err != nil || !replay {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	record, _ = fixture.store.Profile(context.Background(), id)
	if record.Profile.Revision != 1 || requested != 1 {
		t.Fatalf("replay changed state: revision=%d notified=%d", record.Profile.Revision, requested)
	}
	stale := RevalidateInput{ID: id, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}
	var conflict *domain.ConflictError
	if _, err := fixture.profiles.Revalidate(context.Background(), stale); !errors.As(err, &conflict) {
		t.Fatalf("stale version err = %v", err)
	}
	events, _, _ := fixture.store.AuditEvents(context.Background(), ports.AuditFilter{Action: domain.ActionProfileRevalidationRequested, Limit: 10})
	if len(events) != 1 {
		t.Fatalf("revalidation audits = %d", len(events))
	}
}

// T131：不可达的验证结果不得抹掉实例上一次成功时间。
func TestUnreachableValidationKeepsLastSuccess(t *testing.T) {
	fixture := newFeatureFixture(t)
	id := registerCompatibleProfile(t, fixture)
	before, _ := fixture.store.ManagedInstance(context.Background())
	if before.LastSuccessAt == nil {
		t.Fatal("expected last success after compatible validation")
	}
	fixture.adapter.Available = false
	fixture.clock.Advance(1)
	if err := fixture.profiles.RunValidation(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	after, _ := fixture.store.ManagedInstance(context.Background())
	if after.HealthState != "unreachable" || after.LastSuccessAt == nil || !after.LastSuccessAt.Equal(*before.LastSuccessAt) {
		t.Fatalf("instance after unreachable validation = %#v", after)
	}
}
