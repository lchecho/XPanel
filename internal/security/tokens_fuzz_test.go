package security

import (
	"strings"
	"testing"
)

func FuzzStatisticsIDNeverPanicsAndStaysInNamespace(f *testing.F) {
	f.Add("550e8400-e29b-41d4-a716-446655440000")
	f.Add("")
	f.Add("not-a-uuid")
	f.Add("550e8400-e29b-41d4-a716-44665544000>>>")
	f.Fuzz(func(t *testing.T, input string) {
		id, err := StatisticsID(input)
		if err != nil {
			return
		}
		if !strings.HasPrefix(id, "xpanel-") || strings.Contains(id, ">>>") || len(id) != len("xpanel-")+36 {
			t.Fatalf("accepted identity %q violates the namespace contract", id)
		}
		parsed, direction, err := ParseCounterName("user>>>" + id + ">>>traffic>>>uplink")
		if err != nil || parsed != id || direction != "uplink" {
			t.Fatalf("counter round trip failed for %q", id)
		}
	})
}

func FuzzValidateUserKeyNeverPanics(f *testing.F) {
	f.Add(MethodAES256, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	f.Add(MethodAES128, "AAAAAAAAAAAAAAAAAAAAAA==")
	f.Add("unknown", "x")
	f.Fuzz(func(t *testing.T, method, key string) {
		err := ValidateUserKey(method, key)
		if method != MethodAES128 && method != MethodAES256 && err == nil {
			t.Fatalf("unsupported method %q accepted", method)
		}
	})
}

func FuzzParseCounterName(f *testing.F) {
	f.Add("user>>>xpanel-a>>>traffic>>>uplink")
	f.Add("user>>>>>>traffic>>>downlink")
	f.Add("inbound>>>managed>>>traffic>>>uplink")
	f.Fuzz(func(t *testing.T, name string) {
		id, direction, err := ParseCounterName(name)
		if err != nil {
			return
		}
		if id == "" || (direction != "uplink" && direction != "downlink") || strings.Contains(id, ">>>") {
			t.Fatalf("accepted counter %q produced id=%q direction=%q", name, id, direction)
		}
	})
}
