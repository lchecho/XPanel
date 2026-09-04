package domain

import (
	"testing"
	"time"

	"xpanel/internal/security"
)

func TestAccessProfileValidation(t *testing.T) {
	id, _ := NewID()
	instance, _ := NewID()
	profile, err := NewAccessProfile(id, instance, " Main ", "ss2022", "example.com", 8388,
		security.MethodAES256, NetworkTCPUDP, "bootstrap", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if profile.NormalizedName != "main" || profile.Compatibility != CompatibilityUnverified {
		t.Fatalf("profile = %#v", profile)
	}
	if _, err := NewAccessProfile(id, instance, "bad", "tag", "host", 8388, "chacha20", NetworkTCP, "bootstrap", time.Now()); err == nil {
		t.Fatal("unsupported method accepted")
	}
}
