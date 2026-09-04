package security

import (
	"bytes"
	"testing"
)

func TestAEADRoundTripAndAAD(t *testing.T) {
	keyring, err := NewKeyring(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	aad := SecretAAD("credential", "row", "key", 1)
	ciphertext, nonce, err := keyring.Encrypt([]byte("secret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := keyring.Decrypt(ciphertext, nonce, aad)
	if err != nil || string(plaintext) != "secret" {
		t.Fatalf("decrypt = %q, %v", plaintext, err)
	}
	if _, err := keyring.Decrypt(ciphertext, nonce, SecretAAD("credential", "other", "key", 1)); err == nil {
		t.Fatal("AAD mismatch was accepted")
	}
	verifier, verifierNonce, err := keyring.NewVerifier()
	if err != nil || keyring.Verify(verifier, verifierNonce) != nil {
		t.Fatal("verifier round trip failed")
	}
}
