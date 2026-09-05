package xray

import (
	"strings"
	"testing"

	"xpanel/internal/ports"
	"xpanel/internal/security"
)

func FuzzCounterNameRoundTrip(f *testing.F) {
	f.Add("xpanel-550e8400-e29b-41d4-a716-446655440000", true)
	f.Add("bootstrap", false)
	f.Add("", true)
	f.Add("a>>>b", false)
	f.Fuzz(func(t *testing.T, identity string, uplink bool) {
		direction := ports.Downlink
		if uplink {
			direction = ports.Uplink
		}
		name, err := CounterName(identity, direction)
		if err != nil {
			if identity != "" {
				t.Fatalf("non-empty identity %q rejected", identity)
			}
			return
		}
		if !strings.HasPrefix(name, "user>>>") || !strings.HasSuffix(name, ">>>traffic>>>"+string(direction)) {
			t.Fatalf("counter name %q is not exact", name)
		}
		if strings.Contains(identity, ">>>") {
			return // 含分隔符的身份无法无损往返，adapter 层已在领域校验中拒绝。
		}
		parsed, parsedDirection, err := security.ParseCounterName(name)
		if err != nil || parsed != identity || parsedDirection != string(direction) {
			t.Fatalf("round trip failed for %q: %q %q %v", identity, parsed, parsedDirection, err)
		}
	})
}
