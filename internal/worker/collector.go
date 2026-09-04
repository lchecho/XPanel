package worker

import (
	"context"
	"log/slog"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/logging"
)

// 核心函数：Collector 按 traffic_interval 定时执行流量采集。
// 职责：定时调用 TrafficService.CollectOnce 并记录每轮结果；不负责计量规则本身。
// 约束：RPC 在事务外（由 TrafficService 保证）；间隔大于 5 秒时由配置层已发出超额风险警告。
type Collector struct {
	traffic  *application.TrafficService
	interval time.Duration
	logger   *slog.Logger
}

func NewCollector(traffic *application.TrafficService, interval time.Duration, logger *slog.Logger) *Collector {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Collector{traffic: traffic, interval: interval, logger: logger.With(logging.FieldComponent, "collector")}
}

func (c *Collector) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	c.RunOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.RunOnce(ctx)
		}
	}
}

// RunOnce 执行一轮采集；错误已由 TrafficService 记录到实例健康状态，这里只写日志。
func (c *Collector) RunOnce(ctx context.Context) {
	started := time.Now()
	summary, err := c.traffic.CollectOnce(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.logger.Warn("collection failed", logging.FieldResult, "failed", "targets", summary.Targets,
				logging.FieldDurationMS, time.Since(started).Milliseconds())
		}
		return
	}
	c.logger.Debug("collection finished", "targets", summary.Targets, "applied", summary.Applied, "blocked", summary.Blocked,
		logging.FieldDurationMS, time.Since(started).Milliseconds())
}
