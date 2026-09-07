package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"xpanel/internal/testsupport"
)

// 性能回归护栏（非 SC 证明）：20 个活跃分配下页面与采集提交的耗时上限；真实条件下的 SC-002/SC-003 在 validation-report.md 记录。
func TestPerformanceBudgetWithTwentyAllocations(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	templateID := app.RegisterCompatibleTemplate("Primary")
	limit := int64(10 << 30)
	for i := 0; i < 20; i++ {
		record := app.CreateUser(fmt.Sprintf("User %02d", i), templateID, &limit)
		app.SetTraffic(record, uint64(i+1)*1024, uint64(i+1)*4096)
	}
	app.Drain()
	started := time.Now()
	summary, err := app.Traffic.CollectOnce(context.Background())
	collect := time.Since(started)
	if err != nil || summary.Applied != 20 {
		t.Fatalf("collect = %#v, %v", summary, err)
	}
	budgets := map[string]time.Duration{"/": 2 * time.Second, "/users": 2 * time.Second, "/fragments/dashboard-summary": 2 * time.Second,
		"/fragments/users-table": 2 * time.Second, "/audit": 2 * time.Second}
	for path, budget := range budgets {
		started := time.Now()
		response, _ := app.Get(path)
		elapsed := time.Since(started)
		if response.StatusCode != 200 || elapsed > budget {
			t.Fatalf("%s status=%d elapsed=%s budget=%s", path, response.StatusCode, elapsed, budget)
		}
		t.Logf("PERF %s: %s (budget %s)", path, elapsed, budget)
	}
	if collect > 500*time.Millisecond {
		t.Fatalf("collection round for 20 allocations took %s", collect)
	}
	t.Logf("PERF collection round (20 allocations, one short transaction): %s", collect)
}
