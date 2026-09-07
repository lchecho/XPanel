package views

import (
	"strings"
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

// compatibilityReasons 把适配器产生的稳定英文原因翻译成面向管理员的中文说明。
// 未收录的原因原样展示（仍然是脱敏摘要），不隐藏信息。
var compatibilityReasons = map[string]string{
	"unsupported Shadowsocks 2022 method":                            "节点不支持所选的 Shadowsocks 2022 加密方式",
	"node does not satisfy the Shadowsocks 2022 multi-user contract": "节点不支持 Shadowsocks 2022 多用户身份，无法为每个用户下发独立密钥",
	"node created the probe inbound but could not remove it":         "节点能创建入站但无法移除：停用、删除与配额封禁都将无法生效，请检查 Xray 的 HandlerService 权限",
	"template validation failed":                                     "入站模板校验失败，请稍后重试或检查节点状态",
}

// CompatibilityReasonSentence 返回可直接展示给管理员的中文不兼容原因。
func CompatibilityReasonSentence(reason string) string {
	if reason == "" {
		return ""
	}
	if translated, ok := compatibilityReasons[reason]; ok {
		return translated
	}
	const probePrefix = "node could not create a probe inbound: "
	if strings.HasPrefix(reason, probePrefix) {
		return "节点无法创建探针入站：" + strings.TrimPrefix(reason, probePrefix)
	}
	return reason
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
