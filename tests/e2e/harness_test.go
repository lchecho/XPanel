package e2e

import (
	"testing"

	"xpanel/internal/testsupport"
)

// newHarness 返回完整装配的应用夹具（临时 SQLite、fake Xray、固定时钟、HTTP 服务与 Cookie 客户端）。
func newHarness(t *testing.T) *testsupport.App { return testsupport.New(t) }
