package domain

import (
	"testing"
	"time"
)

func TestNextBackoffAndSupersede(t *testing.T) {
	if got := NextBackoff(10, 30*time.Second, func(n int64) int64 { return n - 1 }); got != 30*time.Second {
		t.Fatalf("backoff = %s", got)
	}
	id, _ := NewID()
	allocation, _ := NewID()
	now := time.Now()
	op := NewSynchronizationOperation(id, allocation, 1, true, nil, SyncCreate, SyncAddDesired, now)
	if !op.Supersede(2, now.Add(time.Second)) || op.State != SyncSuperseded {
		t.Fatalf("operation was not superseded: %#v", op)
	}
}
