package application

import (
	"context"
	"time"

	"xpanel/internal/ports"
)

// 核心函数：SettingsService 读取面板级设置（全局配额时区）。
type SettingsService struct {
	store ports.Store
}

func NewSettingsService(store ports.Store) *SettingsService { return &SettingsService{store: store} }

func (s *SettingsService) Get(ctx context.Context) (ports.PanelSettingsRecord, error) {
	return s.store.Settings(ctx)
}

// Location 返回面板配额时区；加载失败时退回 UTC，避免页面渲染中断。
func (s *SettingsService) Location(ctx context.Context) *time.Location {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return time.UTC
	}
	location, err := time.LoadLocation(settings.QuotaTimezone)
	if err != nil {
		return time.UTC
	}
	return location
}
