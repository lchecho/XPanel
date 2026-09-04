package security

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	MethodAES128 = "2022-blake3-aes-128-gcm"
	MethodAES256 = "2022-blake3-aes-256-gcm"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type RedactedString struct{ value string }

func NewRedactedString(value string) RedactedString { return RedactedString{value: value} }
func (RedactedString) String() string               { return "[REDACTED]" }
func (RedactedString) GoString() string             { return "[REDACTED]" }
func (r RedactedString) Reveal() string             { return r.value }

func RandomToken(bytes int) (RedactedString, error) {
	if bytes < 16 {
		return RedactedString{}, errors.New("token entropy must be at least 16 bytes")
	}
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return RedactedString{}, errors.New("generate random token")
	}
	return NewRedactedString(base64.RawURLEncoding.EncodeToString(value)), nil
}

func GenerateUserKey(method string) (RedactedString, error) {
	length, err := methodKeyLength(method)
	if err != nil {
		return RedactedString{}, err
	}
	key := make([]byte, length)
	if _, err := rand.Read(key); err != nil {
		return RedactedString{}, errors.New("generate Shadowsocks user key")
	}
	return NewRedactedString(base64.StdEncoding.EncodeToString(key)), nil
}

func ValidateUserKey(method, encoded string) error {
	length, err := methodKeyLength(method)
	if err != nil {
		return err
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != length {
		return fmt.Errorf("invalid key for %s", method)
	}
	return nil
}

func methodKeyLength(method string) (int, error) {
	switch method {
	case MethodAES128:
		return 16, nil
	case MethodAES256:
		return 32, nil
	default:
		return 0, errors.New("unsupported Shadowsocks 2022 method")
	}
}

func StatisticsID(allocationID string) (string, error) {
	allocationID = strings.ToLower(allocationID)
	if !uuidPattern.MatchString(allocationID) {
		return "", errors.New("allocation ID must be a UUIDv4")
	}
	value := "xpanel-" + allocationID
	if strings.Contains(value, ">>>") {
		return "", errors.New("statistics identity contains a reserved delimiter")
	}
	return value, nil
}

func ParseCounterName(name string) (statisticsID, direction string, err error) {
	parts := strings.Split(name, ">>>")
	if len(parts) != 4 || parts[0] != "user" || parts[1] == "" || parts[2] != "traffic" || (parts[3] != "uplink" && parts[3] != "downlink") {
		return "", "", errors.New("invalid Xray user counter name")
	}
	return parts[1], parts[3], nil
}
