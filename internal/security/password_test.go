package security

import "testing"

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword([]byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := ComparePassword(hash, []byte("correct horse battery staple"))
	if err != nil || !ok {
		t.Fatalf("compare = %v, %v", ok, err)
	}
	ok, err = ComparePassword(hash, []byte("incorrect password"))
	if err != nil || ok {
		t.Fatalf("wrong password compare = %v, %v", ok, err)
	}
}

func TestNormalizeAndPolicy(t *testing.T) {
	username, err := NormalizeUsername("  Ａdmin  ")
	if err != nil || username != "admin" {
		t.Fatalf("username = %q, %v", username, err)
	}
	if err := ValidatePassword(username, []byte("admin")); err == nil {
		t.Fatal("short/equal password accepted")
	}
	if err := ValidatePassword(username, []byte("a secure password")); err != nil {
		t.Fatal(err)
	}
}
