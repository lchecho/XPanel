package views

import (
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

type Option struct{ Value, Label string }

// AuditRow 是审计列表的一行；SafeSummary 已由写入方脱敏。
type AuditRow struct {
	OccurredAt  string
	ActorLabel  string
	TargetLabel string
	TargetLink  string
	ActionLabel string
	ResultLabel string
	Summary     string
}

var auditActionLabels = []Option{
	{domain.ActionLogin, "登录"}, {domain.ActionLogout, "登出"}, {domain.ActionAdministratorInitialized, "管理员初始化"},
	{domain.ActionPasswordReset, "密码重置"}, {domain.ActionUserCreated, "创建用户"}, {domain.ActionUserUpdated, "修改用户"},
	{domain.ActionUserEnabled, "启用用户"}, {domain.ActionUserDisabled, "禁用用户"}, {domain.ActionCredentialRotated, "轮换凭证"},
	{domain.ActionUserDeleted, "删除用户"}, {domain.ActionQuotaExceeded, "配额超限"}, {domain.ActionTrafficReset, "手动重置流量"},
	{domain.ActionCycleRestored, "周期恢复"}, {domain.ActionSyncSucceeded, "同步确认"}, {domain.ActionSyncFailed, "同步失败"},
	{domain.ActionTemplateRegistered, "登记入站模板"}, {domain.ActionTemplateUpdated, "修改入站模板"}, {domain.ActionTemplateValidated, "验证入站模板"},
	{domain.ActionSettingsUpdated, "修改设置"}, {domain.ActionReconcileRemovedUnknown, "协调移除未知身份"},
}

var auditResultLabels = []Option{{string(domain.AuditAccepted), "已接受"}, {string(domain.AuditSucceeded), "成功"},
	{string(domain.AuditFailed), "失败"}, {string(domain.AuditSuperseded), "已取代"}}

func AuditActionOptions() []Option { return auditActionLabels }
func AuditResultOptions() []Option { return auditResultLabels }

func ValidAuditAction(value string) bool {
	for _, option := range auditActionLabels {
		if option.Value == value {
			return true
		}
	}
	return value == ""
}

func ValidAuditResult(value string) bool {
	for _, option := range auditResultLabels {
		if option.Value == value {
			return true
		}
	}
	return value == ""
}

func AuditActionLabel(action string) string {
	for _, option := range auditActionLabels {
		if option.Value == action {
			return option.Label
		}
	}
	return action
}

func AuditResultLabel(result domain.AuditResult) string {
	for _, option := range auditResultLabels {
		if option.Value == string(result) {
			return option.Label
		}
	}
	return string(result)
}

func ActorLabel(actor domain.ActorType) string {
	switch actor {
	case domain.ActorAdministrator:
		return "管理员"
	case domain.ActorLocalCLI:
		return "本机命令行"
	default:
		return "系统"
	}
}

func NewAuditRows(events []domain.AuditEvent, users []ports.UserRecord, location *time.Location) []AuditRow {
	names := make(map[domain.ID]string, len(users))
	for _, user := range users {
		names[user.User.ID] = user.User.DisplayName
	}
	result := make([]AuditRow, 0, len(events))
	for _, event := range events {
		row := AuditRow{OccurredAt: FormatTimeValue(event.OccurredAt, location), ActorLabel: ActorLabel(event.ActorType),
			ActionLabel: AuditActionLabel(event.Action), ResultLabel: AuditResultLabel(event.Result), Summary: event.SafeSummary}
		switch event.TargetType {
		case "user":
			if name, ok := names[event.TargetID]; ok {
				row.TargetLabel = "用户 " + name
			} else {
				row.TargetLabel = "用户 " + event.TargetID.String()
			}
			row.TargetLink = "/users/" + event.TargetID.String()
		case "template":
			row.TargetLabel = "入站模板"
			row.TargetLink = "/templates/" + event.TargetID.String()
		case "settings":
			row.TargetLabel = "面板设置"
			row.TargetLink = "/settings"
		default:
			row.TargetLabel = "管理员"
		}
		result = append(result, row)
	}
	return result
}
