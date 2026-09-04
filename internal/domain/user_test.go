package domain

import "testing"

func TestDisplayStatePriority(t *testing.T) {
	active := ManagedUser{Lifecycle: LifecycleActive}
	deleted := ManagedUser{Lifecycle: LifecycleDeleted}
	tests := []struct {
		name       string
		user       ManagedUser
		allocation AccessAllocation
		want       DisplayState
	}{
		{"deleted wins", deleted, AccessAllocation{AdminEnabled: true, QuotaState: QuotaWithinLimit, ProjectionState: ProjectionPresent}, DisplayDeleted},
		{"manual disable pending", active, AccessAllocation{AdminEnabled: false, QuotaState: QuotaExceeded, ProjectionState: ProjectionPresent}, DisplayDisabling},
		{"manual disable", active, AccessAllocation{AdminEnabled: false, QuotaState: QuotaExceeded, ProjectionState: ProjectionAbsent}, DisplayDisabled},
		{"quota pending", active, AccessAllocation{AdminEnabled: true, QuotaState: QuotaExceeded, ProjectionState: ProjectionPending}, DisplayQuotaDisabling},
		{"quota blocked", active, AccessAllocation{AdminEnabled: true, QuotaState: QuotaExceeded, ProjectionState: ProjectionAbsent}, DisplayQuotaExceeded},
		{"enabling", active, AccessAllocation{AdminEnabled: true, QuotaState: QuotaWithinLimit, ProjectionState: ProjectionPending}, DisplayEnabling},
		{"active", active, AccessAllocation{AdminEnabled: true, QuotaState: QuotaWithinLimit, ProjectionState: ProjectionPresent}, DisplayActive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.allocation.DisplayState(tt.user); got != tt.want {
				t.Fatalf("state = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestCredentialConstraints(t *testing.T) {
	credentials := []AccessCredential{{Version: 1, State: CredentialActive, KeyCiphertext: []byte{1}, KeyNonce: []byte{2}},
		{Version: 2, State: CredentialPending, KeyCiphertext: []byte{3}, KeyNonce: []byte{4}}}
	if err := ValidateCredentialSet(credentials); err != nil {
		t.Fatal(err)
	}
	credentials = append(credentials, AccessCredential{Version: 3, State: CredentialPending, KeyCiphertext: []byte{5}, KeyNonce: []byte{6}})
	if err := ValidateCredentialSet(credentials); err == nil {
		t.Fatal("multiple pending credentials accepted")
	}
}
