package security

import (
	"fmt"
	"strings"
	"testing"
)

func TestUserKeys(t *testing.T) {
	for _, method := range []string{MethodAES128, MethodAES256} {
		key, err := GenerateUserKey(method)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateUserKey(method, key.Reveal()); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateUserKey(MethodAES256, "bad"); err == nil {
		t.Fatal("bad key accepted")
	}
}

func TestRedactedStringFormatting(t *testing.T) {
	value := NewRedactedString("super-secret")
	for _, rendered := range []string{fmt.Sprintf("%v", value), fmt.Sprintf("%s", value), fmt.Sprintf("%#v", value)} {
		if strings.Contains(rendered, "super-secret") || !strings.Contains(rendered, "REDACTED") {
			t.Fatalf("unsafe rendering %q", rendered)
		}
	}
}

func TestStatisticsID(t *testing.T) {
	id, err := StatisticsID("550e8400-e29b-41d4-a716-446655440000")
	if err != nil || id != "xpanel-550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("id = %q, %v", id, err)
	}
}
