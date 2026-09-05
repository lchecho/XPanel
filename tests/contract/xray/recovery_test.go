package xray_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	xrayadapter "xpanel/internal/adapter/xray"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// 契约门禁 7–8：重启改变 boot epoch 并丢弃动态用户，只有应启用的用户被重新加入；
// 可能已生效的变更超时后通过读后写收敛且不产生重复用户。
func TestLiveRestartDropsDynamicUsersAndTimeoutConvergesWithoutDuplicates(t *testing.T) {
	runtime := startRuntime(t)
	bin := contractBinary(t)
	profile := ports.RuntimeProfile{InboundTag: "managed", Method: security.MethodAES256, BootstrapStatisticsID: "bootstrap"}
	target := ports.InstanceTarget{APIEndpoint: runtime.api, ExpectedVersion: wantRuntime, RPCTimeout: 500 * time.Millisecond}
	activeID, blockedID := "xpanel-aaaaaaaa-1111-4111-8111-111111111111", "xpanel-bbbbbbbb-2222-4222-8222-222222222222"
	for _, id := range []string{activeID, blockedID} {
		if _, err := runtime.client.AddUser(context.Background(), ports.AddUserCommand{ProfileTag: "managed", StatisticsID: id, CredentialVersion: 1,
			UserKey: security.NewRedactedString(base64.StdEncoding.EncodeToString([]byte(strings.Repeat(id[7:8], 32))))}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := runtime.client.Probe(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	runtime.restart(t, bin)
	after, err := runtime.client.Probe(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !after.BootEpochKnown || !after.BootEpoch.After(before.BootEpoch) {
		t.Fatalf("boot epoch did not advance: before=%s after=%s", before.BootEpoch, after.BootEpoch)
	}
	users, err := runtime.client.ListUsers(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if user.Kind == "managed" {
			t.Fatalf("dynamic user survived the restart: %#v", user)
		}
	}
	// 协调：只重新加入应启用的用户；配额超限者保持缺席。
	if _, err := runtime.client.AddUser(context.Background(), ports.AddUserCommand{ProfileTag: "managed", StatisticsID: activeID, CredentialVersion: 1,
		UserKey: security.NewRedactedString(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32))))}); err != nil {
		t.Fatal(err)
	}
	users, _ = runtime.client.ListUsers(context.Background(), profile)
	managed := 0
	for _, user := range users {
		if user.Kind == "managed" {
			managed++
			if user.StatisticsID != activeID {
				t.Fatalf("unexpected managed user after reconciliation: %s", user.StatisticsID)
			}
		}
	}
	if managed != 1 {
		t.Fatalf("managed users after reconciliation = %d", managed)
	}

	// 门禁 8：极短超时下的变更结果不确定，读后写后再重放不得产生重复。
	impatient, err := xrayadapter.New(ports.InstanceTarget{APIEndpoint: runtime.api, ExpectedVersion: wantRuntime, RPCTimeout: time.Microsecond})
	if err != nil {
		t.Fatal(err)
	}
	defer impatient.Close()
	uncertainID := "xpanel-cccccccc-3333-4333-8333-333333333333"
	command := ports.AddUserCommand{ProfileTag: "managed", StatisticsID: uncertainID, CredentialVersion: 1,
		UserKey: security.NewRedactedString(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 32))))}
	_, uncertainErr := impatient.AddUser(context.Background(), command)
	var adapterErr *ports.AdapterError
	if uncertainErr != nil && (!errors.As(uncertainErr, &adapterErr) || adapterErr.Kind != ports.ErrorDeadlineExceeded) {
		t.Fatalf("unexpected error kind for impatient add: %v", uncertainErr)
	}
	present := false
	users, _ = runtime.client.ListUsers(context.Background(), profile)
	for _, user := range users {
		if user.StatisticsID == uncertainID {
			present = true
		}
	}
	if !present {
		if _, err := runtime.client.AddUser(context.Background(), command); err != nil {
			if !errors.As(err, &adapterErr) || adapterErr.Kind != ports.ErrorUserAlreadyExists {
				t.Fatal(err)
			}
		}
	}
	users, _ = runtime.client.ListUsers(context.Background(), profile)
	count := 0
	for _, user := range users {
		if user.StatisticsID == uncertainID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("uncertain mutation produced %d copies of the user", count)
	}
}
