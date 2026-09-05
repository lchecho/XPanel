package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"xpanel/internal/domain"
)

type CommandForm struct {
	RequestID   domain.ID
	Version     *int64
	Values      map[string]string
	Fingerprint []byte
}

func ParseCommandForm(r *http.Request, sessionID, action, target string) (CommandForm, error) {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/x-www-form-urlencoded" {
		return CommandForm{}, errors.New("unsupported form content type")
	}
	if err := r.ParseForm(); err != nil {
		return CommandForm{}, errors.New("malformed form")
	}
	id := domain.ID(r.PostForm.Get("_request_id"))
	if !id.Valid() {
		return CommandForm{}, errors.New("invalid request identifier")
	}
	var version *int64
	if raw := r.PostForm.Get("_version"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			return CommandForm{}, errors.New("invalid resource version")
		}
		version = &parsed
	}
	values := make(map[string]string)
	for key := range r.PostForm {
		if strings.HasPrefix(key, "_") || strings.Contains(strings.ToLower(key), "password") || strings.Contains(strings.ToLower(key), "key") {
			continue
		}
		values[key] = r.PostForm.Get(key)
	}
	// 规范化载荷：排除每次页面加载都变化的 CSRF 令牌；指纹绑定 session、动作与目标（http.md §General Rules）。
	canonical := url.Values{}
	for key, list := range r.PostForm {
		if key == "_csrf" {
			continue
		}
		canonical[key] = list
	}
	return CommandForm{RequestID: id, Version: version, Values: values, Fingerprint: RequestFingerprint(sessionID, action, target, canonical.Encode())}, nil
}

func RequestFingerprint(sessionID, action, target, canonicalPayload string) []byte {
	sum := sha256.Sum256([]byte(strings.Join([]string{sessionID, action, target, canonicalPayload}, "\x00")))
	return sum[:]
}

func NewRequestID() string {
	id, err := domain.NewID()
	if err != nil {
		return hex.EncodeToString(make([]byte, 16))
	}
	return id.String()
}
