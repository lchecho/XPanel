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
	input := validTemplateInput(t)
	id, err := fixture.templates.RegisterTemplate(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	edit := input
	edit.Name = "Renamed during validation"
	edit.RequestID = appID(t)
	edit.ExpectedRevision = 0
	fixture.adapter.OnValidateTemplate = func() {
		if err := fixture.templates.UpdateTemplate(context.Background(), id, edit); err != nil {
			t.Errorf("concurrent edit: %v", err)
		}
	}
	if err := fixture.templates.RunValidation(context.Background(), id); !errors.Is(err, ErrValidationStale) {
		t.Fatalf("stale validation err = %v", err)
	}
	record, _ := fixture.store.Template(context.Background(), id)
	if record.Template.Compatibility != domain.CompatibilityUnverified || record.Template.Revision != 1 || record.Template.LastValidatedAt != nil {
		t.Fatalf("stale result applied: %#v", record.Template)
	}
	var identities int
	// 模板校验只探测节点能力，不得副作用地登记任何身份（身份只在创建用户时产生）。
	if err := fixture.store.DB().Read.QueryRow(`SELECT COUNT(*) FROM xray_user_identities`).Scan(&identities); err != nil || identities != 0 {
		t.Fatalf("identities registered by template validation = %d, %v", identities, err)
	}
	events, _, _ := fixture.store.AuditEvents(context.Background(), ports.AuditFilter{Action: domain.ActionTemplateValidated, Limit: 10})
	if len(events) != 0 {
		t.Fatalf("stale validation wrote %d audit events", len(events))
	}
	if err := fixture.templates.RunValidation(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	record, _ = fixture.store.Template(context.Background(), id)
	if record.Template.Compatibility != domain.CompatibilityCompatible || record.Template.Revision != 1 {
		t.Fatalf("fresh validation not applied: %#v", record.Template)
	}
}

// T131：手动重新验证按版本条件置回 unverified 并递增 revision；同请求重放幂等，过期版本冲突。
func TestRevalidateIsVersionedAndIdempotent(t *testing.T) {
	fixture := newFeatureFixture(t)
	id := registerCompatibleTemplate(t, fixture)
	requested := 0
	fixture.templates.notify = func(domain.ID) { requested++ }
	request := RevalidateInput{ID: id, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}
	if replay, err := fixture.templates.Revalidate(context.Background(), request); err != nil || replay {
		t.Fatalf("first revalidate replay=%v err=%v", replay, err)
	}
	record, _ := fixture.store.Template(context.Background(), id)
	if record.Template.Compatibility != domain.CompatibilityUnverified || record.Template.Revision != 1 {
		t.Fatalf("after revalidate: %#v", record.Template)
	}
	if replay, err := fixture.templates.Revalidate(context.Background(), request); err != nil || !replay {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	record, _ = fixture.store.Template(context.Background(), id)
	if record.Template.Revision != 1 || requested != 1 {
		t.Fatalf("replay changed state: revision=%d notified=%d", record.Template.Revision, requested)
	}
	stale := RevalidateInput{ID: id, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}
	var conflict *domain.ConflictError
	if _, err := fixture.templates.Revalidate(context.Background(), stale); !errors.As(err, &conflict) {
		t.Fatalf("stale version err = %v", err)
	}
	events, _, _ := fixture.store.AuditEvents(context.Background(), ports.AuditFilter{Action: domain.ActionTemplateRevalidationRequested, Limit: 10})
	if len(events) != 1 {
		t.Fatalf("revalidation audits = %d", len(events))
	}
}

// T131：不可达的验证结果不得抹掉实例上一次成功时间。
func TestUnreachableValidationKeepsLastSuccess(t *testing.T) {
	fixture := newFeatureFixture(t)
	id := registerCompatibleTemplate(t, fixture)
	before, _ := fixture.store.ManagedInstance(context.Background())
	if before.LastSuccessAt == nil {
		t.Fatal("expected last success after compatible validation")
	}
	fixture.adapter.Available = false
	fixture.clock.Advance(1)
	if err := fixture.templates.RunValidation(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	after, _ := fixture.store.ManagedInstance(context.Background())
	if after.HealthState != "unreachable" || after.LastSuccessAt == nil || !after.LastSuccessAt.Equal(*before.LastSuccessAt) {
		t.Fatalf("instance after unreachable validation = %#v", after)
	}
}
