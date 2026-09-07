package views

import (
	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// StateLabel 是 data-model §AccessAllocation 派生状态表的标签文案。
func StateLabel(state domain.DisplayState) string {
	switch state {
	case domain.DisplayDeleted:
		return "已删除"
	case domain.DisplayDisabling:
		return "手动禁用（移除待同步）"
	case domain.DisplayDisabled:
		return "手动禁用"
	case domain.DisplayQuotaDisabling:
		return "配额超限（移除待同步）"
	case domain.DisplayQuotaExceeded:
		return "配额超限"
	case domain.DisplayEnabling:
		return "启用中（待同步）"
	case domain.DisplayActive:
		return "已启用"
	default:
		return string(state)
	}
}

func CompatibilityLabel(state domain.CompatibilityState) string {
	switch state {
	case domain.CompatibilityCompatible:
		return "兼容"
	case domain.CompatibilityIncompatible:
		return "不兼容"
	case domain.CompatibilityUnreachable:
		return "不可达"
	default:
		return "待验证"
	}
}

func HealthLabel(state string) string {
	switch state {
	case "healthy":
		return "健康"
	case "unreachable":
		return "不可达"
	case "incompatible":
		return "不兼容"
	default:
		return "未知"
	}
}

// ErrorSentence 把稳定错误类别映射为固定的管理员文案（http.md §Response Semantics）。
func ErrorSentence(kind string) string {
	switch kind {
	case "":
		return ""
	case ports.ErrorInstanceUnavailable, ports.ErrorDeadlineExceeded:
		return "节点暂时不可达，系统会自动重试"
	case ports.ErrorIncompatibleProfile, ports.ErrorUnsupportedProtocol, ports.ErrorProfileNotFound, ports.ErrorVersionMismatch:
		return "入站模板与节点不兼容，需要运维检查 Xray 配置"
	case ports.ErrorUserAlreadyExists, ports.ErrorUserNotFound:
		return "节点状态与面板不一致，系统正在自动修复"
	default:
		return "节点拒绝了本次操作，已记录审计"
	}
}

// EventSentence 把统计连续性事件类型映射为固定文案。
func EventSentence(eventType string) string {
	switch eventType {
	case "node_restart":
		return "节点已重启，已从重启后的计数重新开始计量"
	case "counter_decrease":
		return "节点计数异常回落，已建立新基线，期间流量可能未计入"
	case "missing":
		return "节点暂时未返回该用户的流量计数"
	case "reappeared":
		return "计数已恢复"
	case "boundary_gap":
		return "采集间隔超过预期，跨边界流量已归入本次采集完成时的周期"
	case "overflow":
		return "计数值超出可表示范围，本次样本已忽略"
	case "baseline":
		return "已建立首次计量基线"
	default:
		return eventType
	}
}
