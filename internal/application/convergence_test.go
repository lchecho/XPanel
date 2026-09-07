package application

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// T142：显示名称去除首尾空白后保存；NFKC/大小写不敏感的规范化名称保持唯一。
func TestDisplayNameIsTrimmedAndNormalizedUniquenessHolds(t *testing.T) {
	fixture := newFeatureFixture(t)
	templateID := registerCompatibleTemplate(t, fixture)
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	id, _, err := users.CreateUser(context.Background(), CreateUserInput{DisplayName: "  Alice Ｅxample  ", TemplateID: templateID, ResetDay: 1, RequestID: appID(t), ActorID: appID(t)})
	if err != nil {
		t.Fatal(err)
	}
	record, _ := fixture.store.User(context.Background(), id)
	if record.User.DisplayName != "Alice Ｅxample" || record.User.NormalizedName != "alice example" {
		t.Fatalf("stored name = %q normalized = %q", record.User.DisplayName, record.User.NormalizedName)
	}
	if _, _, err := users.CreateUser(context.Background(), CreateUserInput{DisplayName: "ALICE   EXAMPLE", TemplateID: templateID, ResetDay: 1, RequestID: appID(t), ActorID: appID(t)}); err == nil {
		t.Fatal("case/width-insensitive duplicate accepted")
	}
	if _, err := users.UpdateUser(context.Background(), UpdateUserInput{ID: id, DisplayName: "\tRenamed \n", LimitBytes: nil, ResetDay: 1, AdminEnabled: true, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}); err != nil {
		t.Fatal(err)
	}
	record, _ = fixture.store.User(context.Background(), id)
	if record.User.DisplayName != "Renamed" {
		t.Fatalf("updated name not trimmed: %q", record.User.DisplayName)
	}
}

// T137：超过 20 个活跃分配时分批读取并合并为同一轮提交；功能不中断。
func TestCollectOnceBatchesBeyondTwentyAllocations(t *testing.T) {
	fixture := newFeatureFixture(t)
	for i := 0; i < 25; i++ {
		record := presentUser(t, fixture, fmt.Sprintf("Batch %02d", i), nil)
		fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, uint64(i+1)*10)
		fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 5)
	}
	service := newTrafficService(fixture, nil)
	summary, err := service.CollectOnce(context.Background())
	if err != nil || summary.Targets != 25 || summary.Applied != 25 {
		t.Fatalf("summary = %#v, %v", summary, err)
	}
	reads := 0
	for _, call := range fixture.adapter.Calls {
		if call.Operation == "read_traffic" {
			reads++
		}
	}
	if reads != 2 {
		t.Fatalf("read_traffic calls = %d, want 2 batches", reads)
	}
	var total int64
	_ = fixture.store.DB().Read.QueryRow(`SELECT SUM(accounted_uplink_bytes) FROM quota_cycles WHERE status='open'`).Scan(&total)
	if total != 10*(25*26/2) {
		t.Fatalf("accounted total = %d", total)
	}
}

// 端口唯一性以监听地址为界：两个模板共用同一监听地址时，端口不得重复分配（FR-008）。
func TestPortUniquenessSpansTemplatesOnSameListenAddress(t *testing.T) {
	fixture := newFeatureFixture(t)
	first := registerCompatibleTemplate(t, fixture)
	input := validTemplateInput(t)
	input.Name = "Secondary"
	input.RequestID = appID(t)
	second, err := fixture.templates.RegisterTemplate(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.templates.RunValidation(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	firstID, _, err := users.CreateUser(context.Background(), CreateUserInput{DisplayName: "A", TemplateID: first,
		ResetDay: 1, RequestID: appID(t), ActorID: appID(t)})
	if err != nil {
		t.Fatal(err)
	}
	record, _ := fixture.store.User(context.Background(), firstID)
	port := record.Inbound.Inbound.Port
	// 在第二个模板上指定同一端口：两模板监听地址相同，必须被拒绝。
	var conflict *domain.ConflictError
	if _, _, err := users.CreateUser(context.Background(), CreateUserInput{DisplayName: "B", TemplateID: second,
		Port: &port, ResetDay: 1, RequestID: appID(t), ActorID: appID(t)}); !errors.As(err, &conflict) {
		t.Fatalf("duplicate port across templates err = %v, want conflict", err)
	}
	// 自动分配会跳过已占用端口。
	secondID, _, err := users.CreateUser(context.Background(), CreateUserInput{DisplayName: "B", TemplateID: second,
		ResetDay: 1, RequestID: appID(t), ActorID: appID(t)})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := fixture.store.User(context.Background(), secondID)
	if other.Inbound.Inbound.Port == port {
		t.Fatalf("auto allocation reused an assigned port: %d", port)
	}
}

// 已有受管用户的模板不得修改加密方式或监听地址；公开地址与端口池仍可修改（FR-006）。
func TestTemplateContractFieldsFrozenWhileUsersExist(t *testing.T) {
	fixture := newFeatureFixture(t)
	record := presentUser(t, fixture, "Pinned", nil)
	current, _ := fixture.store.Template(context.Background(), record.Allocation.TemplateID)
	base := TemplateInput{Name: current.Template.Name, PublicHost: "edge.example.com",
		ListenAddress: current.Template.ListenAddress, PortPoolStart: current.Template.Pool.Start,
		PortPoolEnd: current.Template.Pool.End, Method: current.Template.Method, Network: current.Template.Network,
		RequestID: appID(t), ExpectedRevision: current.Template.Revision, ActorID: appID(t)}
	if err := fixture.templates.UpdateTemplate(context.Background(), current.Template.ID, base); err != nil {
		t.Fatalf("public host edit rejected: %v", err)
	}
	current, _ = fixture.store.Template(context.Background(), record.Allocation.TemplateID)
	for name, mutate := range map[string]func(*TemplateInput){
		"method":         func(in *TemplateInput) { in.Method = security.MethodAES128 },
		"listen address": func(in *TemplateInput) { in.ListenAddress = "0.0.0.0" },
	} {
		input := base
		input.RequestID, input.ExpectedRevision = appID(t), current.Template.Revision
		mutate(&input)
		var conflict *domain.ConflictError
		if err := fixture.templates.UpdateTemplate(context.Background(), current.Template.ID, input); !errors.As(err, &conflict) {
			t.Fatalf("%s change with users error = %v", name, err)
		}
	}
	// 端口池调整不受契约字段冻结影响。
	widened := base
	widened.RequestID, widened.ExpectedRevision = appID(t), current.Template.Revision
	widened.PortPoolEnd = current.Template.Pool.End + 10
	if err := fixture.templates.UpdateTemplate(context.Background(), current.Template.ID, widened); err != nil {
		t.Fatalf("port pool widening rejected: %v", err)
	}
	after, _ := fixture.store.Template(context.Background(), record.Allocation.TemplateID)
	if after.Template.Method != current.Template.Method || after.Template.Compatibility != domain.CompatibilityCompatible {
		t.Fatalf("template mutated despite rejection: %#v", after.Template)
	}
}

// T139：启用中的用户同时命中“启用”业务筛选与“待同步”筛选。
func TestStatusFiltersAreOrthogonal(t *testing.T) {
	fixture := newFeatureFixture(t)
	templateID := registerCompatibleTemplate(t, fixture)
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	if _, _, err := users.CreateUser(context.Background(), CreateUserInput{DisplayName: "Enabling", TemplateID: templateID, ResetDay: 1, RequestID: appID(t), ActorID: appID(t)}); err != nil {
		t.Fatal(err)
	}
	presentUser(t, fixture, "Active", nil)
	for status, want := range map[string]int{"active": 2, "pending": 1, "disabled": 0, "": 2} {
		list, err := users.List(context.Background(), ports.UserFilter{Status: status})
		if err != nil || len(list) != want {
			t.Fatalf("status=%q count=%d want %d (%v)", status, len(list), want, err)
		}
	}
}
