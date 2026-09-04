package security

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const verifierPlaintext = "xpanel-root-key-verifier"

type Keyring struct {
	fieldKey []byte
	csrfKey  []byte
}

func LoadRootKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, errors.New("root key file is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("root key file must be regular and accessible only by its owner")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("read root key file")
	}
	if len(encoded) < 2 || encoded[len(encoded)-1] != '\n' || encoded[len(encoded)-2] == '\n' {
		return nil, errors.New("root key file must contain one Base64 line ending in a newline")
	}
	raw, err := base64.StdEncoding.DecodeString(string(encoded[:len(encoded)-1]))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("root key must decode to exactly 32 bytes")
	}
	return raw, nil
}

func NewKeyring(root []byte) (*Keyring, error) {
	if len(root) != 32 {
		return nil, errors.New("root key must be 32 bytes")
	}
	field, err := derive(root, "xpanel-field-aead-v1", chacha20poly1305.KeySize)
	if err != nil {
		return nil, err
	}
	csrf, err := derive(root, "xpanel-csrf-v1", 32)
	if err != nil {
		return nil, err
	}
	return &Keyring{fieldKey: field, csrfKey: csrf}, nil
}

func derive(root []byte, label string, size int) ([]byte, error) {
	out := make([]byte, size)
	if _, err := io.ReadFull(hkdf.New(sha256.New, root, nil, []byte(label)), out); err != nil {
		return nil, fmt.Errorf("derive %s key: %w", label, err)
	}
	return out, nil
}

func (k *Keyring) CSRFKey() []byte { return append([]byte(nil), k.csrfKey...) }

func (k *Keyring) Encrypt(plaintext, aad []byte) (ciphertext, nonce []byte, err error) {
	aead, err := chacha20poly1305.NewX(k.fieldKey)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, errors.New("generate secret nonce")
	}
	return aead.Seal(nil, nonce, plaintext, aad), nonce, nil
}

func (k *Keyring) Decrypt(ciphertext, nonce, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(k.fieldKey)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, errors.New("secret authentication failed")
	}
	return plaintext, nil
}

func SecretAAD(entity, rowID, field string, version int64) []byte {
	return []byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", entity, rowID, field, version))
}

func (k *Keyring) NewVerifier() (ciphertext, nonce []byte, err error) {
	return k.Encrypt([]byte(verifierPlaintext), SecretAAD("panel_settings", "1", "key_verifier", 1))
}

func (k *Keyring) Verify(ciphertext, nonce []byte) error {
	plaintext, err := k.Decrypt(ciphertext, nonce, SecretAAD("panel_settings", "1", "key_verifier", 1))
	if err != nil || string(plaintext) != verifierPlaintext {
		return errors.New("root_key_mismatch")
	}
	return nil
}
