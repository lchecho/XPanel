package security

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/unicode/norm"
)

const (
	argonMemory      = 64 * 1024
	argonIterations  = 3
	argonParallelism = 4
	argonSaltLength  = 16
	argonKeyLength   = 32
)

func NormalizeUsername(username string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(norm.NFKC.String(username)))
	count := utf8.RuneCountInString(value)
	if count < 1 || count > 64 {
		return "", errors.New("username must contain 1 to 64 characters")
	}
	return value, nil
}

func ValidatePassword(username string, password []byte) error {
	if len(password) < 12 || len(password) > 256 {
		return errors.New("password must contain 12 to 256 bytes")
	}
	normalized, err := NormalizeUsername(username)
	if err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(norm.NFKC.String(string(password))), normalized) {
		return errors.New("password must not equal username")
	}
	return nil
}

func HashPassword(password []byte) (string, error) {
	salt := make([]byte, argonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", errors.New("generate password salt")
	}
	hash := argon2.IDKey(password, salt, argonIterations, argonMemory, argonParallelism, argonKeyLength)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonIterations,
		argonParallelism, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func ComparePassword(encoded string, password []byte) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false, errors.New("invalid password hash format")
	}
	var memory uint64
	var iterations uint64
	var parallelism uint64
	for _, item := range strings.Split(parts[3], ",") {
		pair := strings.SplitN(item, "=", 2)
		if len(pair) != 2 {
			return false, errors.New("invalid password hash parameters")
		}
		value, err := strconv.ParseUint(pair[1], 10, 32)
		if err != nil {
			return false, errors.New("invalid password hash parameters")
		}
		switch pair[0] {
		case "m":
			memory = value
		case "t":
			iterations = value
		case "p":
			parallelism = value
		default:
			return false, errors.New("invalid password hash parameters")
		}
	}
	if memory == 0 || iterations == 0 || parallelism == 0 || parallelism > 255 {
		return false, errors.New("invalid password hash parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return false, errors.New("invalid password hash salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, errors.New("invalid password hash value")
	}
	got := argon2.IDKey(password, salt, uint32(iterations), uint32(memory), uint8(parallelism), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
