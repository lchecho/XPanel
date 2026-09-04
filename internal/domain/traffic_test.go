package domain

import (
	"testing"
	"time"
)

func counter(value int64) *int64 { return &value }

func TestApplySampleRules(t *testing.T) {
	first := ApplySample("uplink", TrafficDirectionCursor{}, TrafficSample{Found: true, Bytes: 500}, false, false)
	if first.Delta != 500 || *first.Cursor.Counter != 500 || len(first.Events) != 1 || first.Events[0].Type != EventBaseline {
		t.Fatalf("first sample = %#v", first)
	}
	growth := ApplySample("uplink", TrafficDirectionCursor{Counter: counter(500), Epoch: 0}, TrafficSample{Found: true, Bytes: 800}, false, false)
	if growth.Delta != 300 || len(growth.Events) != 0 || growth.Cursor.Epoch != 0 {
		t.Fatalf("growth = %#v", growth)
	}
	restart := ApplySample("uplink", TrafficDirectionCursor{Counter: counter(800), Epoch: 0}, TrafficSample{Found: true, Bytes: 120}, true, false)
	if restart.Delta != 120 || restart.Cursor.Epoch != 1 || restart.Events[0].Type != EventNodeRestart {
		t.Fatalf("restart = %#v", restart)
	}
	decrease := ApplySample("uplink", TrafficDirectionCursor{Counter: counter(800), Epoch: 0}, TrafficSample{Found: true, Bytes: 120}, false, false)
	if decrease.Delta != 0 || decrease.Cursor.Epoch != 1 || *decrease.Cursor.Counter != 120 || decrease.Events[0].Type != EventCounterDecrease {
		t.Fatalf("decrease = %#v", decrease)
	}
	missing := ApplySample("uplink", TrafficDirectionCursor{Counter: counter(800)}, TrafficSample{Found: false}, false, false)
	if !missing.Missing || missing.Delta != 0 || *missing.Cursor.Counter != 800 || missing.Events[0].Type != EventMissing {
		t.Fatalf("missing = %#v", missing)
	}
	stillMissing := ApplySample("uplink", TrafficDirectionCursor{Counter: counter(800)}, TrafficSample{Found: false}, false, true)
	if len(stillMissing.Events) != 0 {
		t.Fatal("repeated missing must not spam events")
	}
	neverSeen := ApplySample("uplink", TrafficDirectionCursor{}, TrafficSample{Found: false}, false, false)
	if len(neverSeen.Events) != 0 || !neverSeen.Missing {
		t.Fatalf("missing before first traffic = %#v", neverSeen)
	}
	reappeared := ApplySample("uplink", TrafficDirectionCursor{Counter: counter(800)}, TrafficSample{Found: true, Bytes: 900}, false, true)
	if reappeared.Delta != 100 || reappeared.Events[0].Type != EventReappeared {
		t.Fatalf("reappeared = %#v", reappeared)
	}
	overflow := ApplySample("uplink", TrafficDirectionCursor{Counter: counter(800)}, TrafficSample{Found: true, Bytes: 1 << 63}, false, false)
	if overflow.Delta != 0 || overflow.Cursor.Epoch != 1 || *overflow.Cursor.Counter != 800 || overflow.Events[0].Type != EventOverflow {
		t.Fatalf("overflow = %#v", overflow)
	}
}

func TestDeltaIsNeverNegative(t *testing.T) {
	for previous := int64(0); previous < 5000; previous += 700 {
		for current := uint64(0); current < 5000; current += 900 {
			for _, restart := range []bool{false, true} {
				result := ApplySample("downlink", TrafficDirectionCursor{Counter: counter(previous)}, TrafficSample{Found: true, Bytes: current}, restart, false)
				if result.Delta < 0 {
					t.Fatalf("negative delta for previous=%d current=%d restart=%v", previous, current, restart)
				}
			}
		}
	}
}

func TestRestartConfirmedAndBoundaryGap(t *testing.T) {
	stored := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	if RestartConfirmed(stored, stored.Add(3*time.Second), true, 5*time.Second) {
		t.Fatal("clock jitter within one interval treated as restart")
	}
	if !RestartConfirmed(stored, stored.Add(time.Minute), true, 5*time.Second) {
		t.Fatal("epoch shift beyond one interval not treated as restart")
	}
	if RestartConfirmed(stored, stored.Add(time.Minute), false, 5*time.Second) || RestartConfirmed(time.Time{}, stored, true, 5*time.Second) {
		t.Fatal("unknown or unset epoch must not confirm a restart")
	}
	if BoundaryGap(stored, stored.Add(9*time.Second), 5*time.Second) || !BoundaryGap(stored, stored.Add(11*time.Second), 5*time.Second) {
		t.Fatal("boundary gap threshold is wrong")
	}
	if _, err := AddDelta(MaxInt64, 1); err == nil {
		t.Fatal("aggregate overflow accepted")
	}
}
