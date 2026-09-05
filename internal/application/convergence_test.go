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
	profileID := registerCompatibleProfile(t, fixture)
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	id, _, err := users.CreateUser(context.Background(), CreateUserInput{DisplayName: "  Alice Ｅxample  ", ProfileID: profileID, ResetDay: 1, RequestID: appID(t), ActorID: appID(t)})
	if err != nil {
		t.Fatal(err)
	}
	record, _ := fixture.store.User(context.Background(), id)
	if record.User.DisplayName != "Alice Ｅxample" || record.User.NormalizedName != "alice example" {
		t.Fatalf("stored name = %q normalized = %q", record.User.DisplayName, record.User.NormalizedName)
	}
	if _, _, err := users.CreateUser(context.Background(), CreateUserInput{DisplayName: "ALICE   EXAMPLE", ProfileID: profileID, ResetDay: 1, RequestID: appID(t), ActorID: appID(t)}); err == nil {
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

// T134：跨 profile 复用同一 bootstrap 统计标识时第二个 profile 必须不兼容；同 profile 重放幂等。
func TestBootstrapIdentityConflictAcrossProfiles(t *testing.T) {
	fixture := newFeatureFixture(t)
	first := registerCompatibleProfile(t, fixture)
	if err := fixture.profiles.RunValidation(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	input := validProfileInput(t)
	input.Name, input.InboundTag = "Secondary", "other"
	second, err := fixture.profiles.RegisterProfile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.profiles.RunValidation(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	record, _ := fixture.store.Profile(context.Background(), second)
	if record.Profile.Compatibility != domain.CompatibilityIncompatible || record.Profile.CompatibilityReason == "" {
		t.Fatalf("second profile = %s %q", record.Profile.Compatibility, record.Profile.CompatibilityReason)
	}
	var identities int
	_ = fixture.store.DB().Read.QueryRow(`SELECT count(*) FROM xray_user_identities WHERE kind='bootstrap'`).Scan(&identities)
	if identities != 1 {
		t.Fatalf("bootstrap identities = %d", identities)
	}
}

// T133：已有受管用户的 profile 不得修改入站标签、加密方式或 bootstrap 标识；公开地址仍可修改。
func TestProfileContractFieldsFrozenWhileUsersExist(t *testing.T) {
	fixture := newFeatureFixture(t)
	record := presentUser(t, fixture, "Pinned", nil)
	current, _ := fixture.store.Profile(context.Background(), record.Profile.Profile.ID)
	base := ProfileInput{Name: current.Profile.Name, InboundTag: current.Profile.InboundTag, PublicHost: "edge.example.com", PublicPort: 8388,
		Method: current.Profile.Method, Network: current.Profile.Network, BootstrapStatisticsID: current.Profile.BootstrapStatisticsID,
		RequestID: appID(t), ExpectedRevision: current.Profile.Revision, ActorID: appID(t)}
	if err := fixture.profiles.UpdateProfile(context.Background(), current.Profile.ID, base); err != nil {
		t.Fatalf("public host edit rejected: %v", err)
	}
	current, _ = fixture.store.Profile(context.Background(), record.Profile.Profile.ID)
	for name, mutate := range map[string]func(*ProfileInput){
		"inbound tag": func(in *ProfileInput) { in.InboundTag = "moved" },
		"method":      func(in *ProfileInput) { in.Method = security.MethodAES128 },
		"bootstrap":   func(in *ProfileInput) { in.BootstrapStatisticsID = "other-bootstrap" },
	} {
		input := base
		input.RequestID, input.ExpectedRevision = appID(t), current.Profile.Revision
		mutate(&input)
		var conflict *domain.ConflictError
		if err := fixture.profiles.UpdateProfile(context.Background(), current.Profile.ID, input); !errors.As(err, &conflict) {
			t.Fatalf("%s change with users error = %v", name, err)
		}
	}
	after, _ := fixture.store.Profile(context.Background(), record.Profile.Profile.ID)
	if after.Profile.InboundTag != current.Profile.InboundTag || after.Profile.Compatibility != domain.CompatibilityCompatible {
		t.Fatalf("profile mutated despite rejection: %#v", after.Profile)
	}
}

// T139：启用中的用户同时命中“启用”业务筛选与“待同步”筛选。
func TestStatusFiltersAreOrthogonal(t *testing.T) {
	fixture := newFeatureFixture(t)
	profileID := registerCompatibleProfile(t, fixture)
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	if _, _, err := users.CreateUser(context.Background(), CreateUserInput{DisplayName: "Enabling", ProfileID: profileID, ResetDay: 1, RequestID: appID(t), ActorID: appID(t)}); err != nil {
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
