package application

import (
	"context"
	"errors"
	"testing"

	"xpanel/internal/domain"
)

func TestCreateUserValidationReplayAndOfflineAcceptance(t *testing.T) {
	fixture := newFeatureFixture(t)
	profileID := registerCompatibleProfile(t, fixture)
	wakes := 0
	service := NewUserService(fixture.store, fixture.keyring, fixture.clock, func() { wakes++ })
	limit := int64(1024)
	input := CreateUserInput{DisplayName: "Alice", ProfileID: profileID, LimitBytes: &limit, ResetDay: 1,
		RequestID: appID(t), ActorID: appID(t)}
	id, replay, err := service.CreateUser(context.Background(), input)
	if err != nil || replay || wakes != 1 {
		t.Fatalf("create = %s, %v, %v; wakes=%d", id, replay, err, wakes)
	}
	replayID, replay, err := service.CreateUser(context.Background(), input)
	if err != nil || !replay || replayID != id || wakes != 1 {
		t.Fatalf("replay = %s, %v, %v; wakes=%d", replayID, replay, err, wakes)
	}
	duplicate := input
	duplicate.RequestID = appID(t)
	if _, _, err := service.CreateUser(context.Background(), duplicate); err == nil {
		t.Fatal("duplicate name accepted")
	}
	zero := int64(0)
	invalid := input
	invalid.DisplayName, invalid.RequestID, invalid.LimitBytes = "Bob", appID(t), &zero
	if _, _, err := service.CreateUser(context.Background(), invalid); err == nil {
		t.Fatal("zero quota accepted")
	}

	fixture.adapter.Available = false
	offline := input
	offline.DisplayName, offline.RequestID = "Offline", appID(t)
	offlineID, _, err := service.CreateUser(context.Background(), offline)
	if err != nil {
		t.Fatalf("offline create should persist desired state: %v", err)
	}
	record, err := fixture.store.User(context.Background(), offlineID)
	if err != nil || !record.Allocation.PendingSync() || record.Allocation.ProjectionState != domain.ProjectionPending {
		t.Fatalf("offline record = %#v, %v", record, err)
	}
}

func TestCreateUserRejectsIncompatibleProfile(t *testing.T) {
	fixture := newFeatureFixture(t)
	input := validProfileInput(t)
	profileID, err := fixture.profiles.RegisterProfile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	service := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	_, _, err = service.CreateUser(context.Background(), CreateUserInput{DisplayName: "Alice", ProfileID: profileID,
		ResetDay: 1, RequestID: appID(t), ActorID: appID(t)})
	var stateErr *domain.InvalidStateError
	if !errors.As(err, &stateErr) {
		t.Fatalf("incompatible create error = %T %v", err, err)
	}
}
