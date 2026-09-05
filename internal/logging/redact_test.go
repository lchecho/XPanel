package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestSensitiveKeysAndConnectionURIsAreRedacted(t *testing.T) {
	var buffer bytes.Buffer
	logger := New(&buffer, slog.LevelDebug)
	logger.Info("user connected via ss://YWVzOnNlY3JldA==@vpn.example.com:8388#alice",
		"password", "hunter2hunter2", "server_key", "c2VjcmV0", "session_token", "tok", "csrf", "x",
		"allocation_id", "550e8400-e29b-41d4-a716-446655440000", "result", "succeeded")
	line := buffer.String()
	for _, secret := range []string{"hunter2hunter2", "c2VjcmV0", "ss://", "YWVzOnNlY3JldA=="} {
		if strings.Contains(line, secret) {
			t.Fatalf("log line leaked %q: %s", secret, line)
		}
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("log line is not JSON: %s", line)
	}
	if record["password"] != "[REDACTED]" || record["server_key"] != "[REDACTED]" || record["session_token"] != "[REDACTED]" {
		t.Fatalf("sensitive attributes not redacted: %s", line)
	}
	// 分配标识是标识符而非凭证，必须保留以支持关联排障（宪章 V 的 UUID 脱敏指凭证型 UUID）。
	if record["allocation_id"] != "550e8400-e29b-41d4-a716-446655440000" || record["result"] != "succeeded" {
		t.Fatalf("identifier fields were damaged: %s", line)
	}
	if !strings.Contains(record["msg"].(string), "[REDACTED_URI]") {
		t.Fatalf("connection URI not redacted in message: %s", line)
	}
}

func TestRedactHelpers(t *testing.T) {
	for _, key := range []string{"password", "Passwd", "SERVER_KEY", "csrf_token", "authorization", "cookie"} {
		if !IsSensitiveKey(key) {
			t.Fatalf("%s should be sensitive", key)
		}
	}
	for _, key := range []string{"allocation_id", "result", "duration_ms", "node_id"} {
		if IsSensitiveKey(key) {
			t.Fatalf("%s should not be sensitive", key)
		}
	}
	if RedactValue("token", "abc") != "[REDACTED]" || RedactValue("count", 3) != 3 {
		t.Fatal("RedactValue behaves unexpectedly")
	}
	if RedactText("counter user>>>xpanel-a>>>traffic>>>uplink grew") != "counter user>>>xpanel-a>>>traffic>>>uplink grew" {
		t.Fatal("counter names must survive redaction")
	}
}
