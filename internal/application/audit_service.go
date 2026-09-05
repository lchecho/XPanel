package application

import (
	"context"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// 核心函数：AuditService 提供只读审计查询；审计表 append-only，本服务不暴露任何修改路径。
type AuditService struct {
	store ports.Store
}

func NewAuditService(store ports.Store) *AuditService { return &AuditService{store: store} }

func (s *AuditService) List(ctx context.Context, filter ports.AuditFilter) ([]domain.AuditEvent, *ports.AuditCursor, error) {
	return s.store.AuditEvents(ctx, filter)
}
