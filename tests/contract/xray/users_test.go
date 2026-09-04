package xray_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"xpanel/internal/ports"
	"xpanel/internal/security"
)

func TestLiveProfileAndUserMutationContract(t *testing.T) {
	runtime := startRuntime(t)
	profile := ports.RuntimeProfile{InboundTag: "managed", Method: security.MethodAES256, BootstrapStatisticsID: "bootstrap"}
	capabilities, err := runtime.client.ValidateProfile(context.Background(), profile)
	if err != nil || !capabilities.Compatible() {
		t.Fatalf("fixed runtime profile contract = %#v, %v", capabilities, err)
	}
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("u", 32)))
	command := ports.AddUserCommand{ProfileTag: "managed", StatisticsID: "xpanel-550e8400-e29b-41d4-a716-446655440000",
		CredentialVersion: 1, UserKey: security.NewRedactedString(key)}
	if _, err := runtime.client.AddUser(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	users, err := runtime.client.ListUsers(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	managed, bootstrap := 0, 0
	for _, user := range users {
		if user.Kind == "managed" {
			managed++
		}
		if user.Kind == "bootstrap" {
			bootstrap++
		}
	}
	if managed != 1 || bootstrap != 1 {
		t.Fatalf("unexpected runtime identities: %#v", users)
	}
	if _, err := runtime.client.AddUser(context.Background(), command); err == nil {
		t.Fatal("duplicate add unexpectedly succeeded")
	}
	if _, err := runtime.client.RemoveUser(context.Background(), ports.RemoveUserCommand{ProfileTag: "managed", StatisticsID: command.StatisticsID}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.client.RemoveUser(context.Background(), ports.RemoveUserCommand{ProfileTag: "managed", StatisticsID: command.StatisticsID}); err == nil {
		t.Fatal("missing-user removal unexpectedly succeeded")
	}
}
