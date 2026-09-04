package application

import (
	"context"
	"errors"
	"strings"
	"testing"

	"xpanel/internal/domain"
)

func TestConnectionInfoRequiresConfirmedCredential(t *testing.T) {
	fixture := newFeatureFixture(t)
	profileID := registerCompatibleProfile(t, fixture)
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	userID, _, err := users.CreateUser(context.Background(), CreateUserInput{DisplayName: "Alice Example", ProfileID: profileID,
		ResetDay: 1, RequestID: appID(t), ActorID: appID(t)})
	if err != nil {
		t.Fatal(err)
	}
	connections := NewConnectionService(fixture.store, fixture.keyring)
	if _, err := connections.BuildConnectionInfo(context.Background(), userID); !errors.Is(err, ErrConnectionPending) {
		t.Fatalf("unconfirmed connection error = %v", err)
	}
	var operationID string
	if err := fixture.store.DB().Read.QueryRow(`SELECT id FROM synchronization_operations WHERE allocation_id=(SELECT id FROM access_allocations WHERE user_id=?)`, userID.String()).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.User(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	if ok, err := fixture.store.ConfirmIfRevisionCurrent(context.Background(), domain.ID(operationID), 1, 1, true, fixture.clock.Now()); err != nil || !ok {
		t.Fatalf("confirm = %v, %v", ok, err)
	}
	info, err := connections.BuildConnectionInfo(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(info.URI.Reveal(), "ss://") || info.Password.Reveal() == "" || strings.Contains(info.Password.String(), info.Password.Reveal()) {
		t.Fatalf("connection info was not safely assembled: %#v", info)
	}
}
