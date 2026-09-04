package application

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"strconv"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

var ErrConnectionPending = errors.New("connection information is pending synchronization")

type ConnectionInfo struct {
	Host     string
	Port     int
	Method   string
	Label    string
	Password security.RedactedString
	URI      security.RedactedString
	Inactive bool
}

type ConnectionService struct {
	store   ports.Store
	keyring *security.Keyring
}

func NewConnectionService(store ports.Store, keyring *security.Keyring) *ConnectionService {
	return &ConnectionService{store: store, keyring: keyring}
}

func (s *ConnectionService) BuildConnectionInfo(ctx context.Context, userID domain.ID) (ConnectionInfo, error) {
	record, err := s.store.User(ctx, userID)
	if err != nil {
		return ConnectionInfo{}, err
	}
	if record.User.Lifecycle == domain.LifecycleDeleted {
		return ConnectionInfo{}, &domain.InvalidStateError{Message: "connection information is unavailable for deleted users"}
	}
	if record.Credential.State != domain.CredentialActive || record.Allocation.SyncedCredentialVersion == nil ||
		*record.Allocation.SyncedCredentialVersion != record.Credential.Version || record.Allocation.DesiredCredentialVersion != record.Credential.Version {
		return ConnectionInfo{}, ErrConnectionPending
	}
	serverKey, err := s.keyring.Decrypt(record.Profile.ServerKeyCiphertext, record.Profile.ServerKeyNonce,
		security.SecretAAD("access_profiles", record.Profile.Profile.ID.String(), "server_key", record.Profile.KeyEncryptionVersion))
	if err != nil {
		return ConnectionInfo{}, errors.New("profile key is unavailable")
	}
	userKey, err := s.keyring.Decrypt(record.Credential.KeyCiphertext, record.Credential.KeyNonce,
		security.SecretAAD("access_credentials", record.Allocation.ID.String(), "user_key", record.Credential.KeyEncryptionVersion))
	if err != nil {
		return ConnectionInfo{}, errors.New("credential key is unavailable")
	}
	password := string(serverKey) + ":" + string(userKey)
	userinfo := base64.RawURLEncoding.EncodeToString([]byte(record.Profile.Profile.Method + ":" + password))
	hostport := net.JoinHostPort(record.Profile.Profile.PublicHost, strconv.Itoa(record.Profile.Profile.PublicPort))
	uri := "ss://" + userinfo + "@" + hostport + "#" + url.PathEscape(record.User.DisplayName)
	return ConnectionInfo{Host: record.Profile.Profile.PublicHost, Port: record.Profile.Profile.PublicPort,
		Method: record.Profile.Profile.Method, Label: record.User.DisplayName,
		Password: security.NewRedactedString(password), URI: security.NewRedactedString(uri),
		Inactive: !record.Allocation.DesiredPresent(record.User)}, nil
}
