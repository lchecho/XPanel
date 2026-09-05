package sqlite

import (
	"context"
	"testing"
	"time"

	"xpanel/internal/domain"
)

func TestSyncLeaseRecoveryAndRevisionGuard(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := userFixture(t, store, now, "Lease User")
	if _, _, err := store.CreateUser(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	work, err := store.LeaseDue(context.Background(), "worker-a", now, time.Second)
	if err != nil || work == nil || work.Operation.ID != record.Operation.ID {
		t.Fatalf("first lease = %#v, %v", work, err)
	}
	if work, err = store.LeaseDue(context.Background(), "worker-b", now.Add(500*time.Millisecond), time.Second); err != nil || work != nil {
		t.Fatalf("live lease was stolen: %#v, %v", work, err)
	}
	work, err = store.LeaseDue(context.Background(), "worker-b", now.Add(2*time.Second), time.Second)
	if err != nil || work == nil {
		t.Fatalf("expired lease was not recovered: %#v, %v", work, err)
	}

	if _, err := store.db.Write.Exec(`UPDATE access_allocations SET desired_revision=2 WHERE id=?`, record.Allocation.ID.String()); err != nil {
		t.Fatal(err)
	}
	confirmed, err := store.ConfirmIfRevisionCurrent(context.Background(), record.Operation.ID, 1, 1, true, now.Add(3*time.Second))
	if err != nil || confirmed {
		t.Fatalf("stale confirmation = %v, %v", confirmed, err)
	}
	var state string
	if err := store.db.Read.QueryRow(`SELECT state FROM synchronization_operations WHERE id=?`, record.Operation.ID.String()).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.SyncSuperseded) {
		t.Fatalf("stale operation state = %s", state)
	}
}

func TestSyncRescheduleAndSupersede(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := userFixture(t, store, now, "Retry User")
	if _, _, err := store.CreateUser(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if work, err := store.LeaseDue(context.Background(), "worker-a", now, time.Second); err != nil || work == nil {
		t.Fatalf("lease = %#v, %v", work, err)
	}
	if err := store.Reschedule(context.Background(), record.Operation.ID, "worker-a", 2, now.Add(5*time.Second), "instance_unavailable", "temporarily unavailable"); err != nil {
		t.Fatal(err)
	}
	var state string
	_ = store.db.Read.QueryRow(`SELECT state FROM synchronization_operations WHERE id=?`, record.Operation.ID.String()).Scan(&state)
	if state != string(domain.SyncRetryWait) {
		t.Fatalf("state after reschedule = %s", state)
	}
	if count, err := store.Supersede(context.Background(), record.Allocation.ID, 2, now.Add(time.Second)); err != nil || count != 1 {
		t.Fatalf("supersede = %d, %v", count, err)
	}
}
