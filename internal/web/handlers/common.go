package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/gorilla/csrf"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/logging"
	webmiddleware "xpanel/internal/web/middleware"
	"xpanel/internal/web/views"
)

const (
	sessionFlashKind    = "flash_kind"
	sessionFlashMessage = "flash_message"
)

// Base 汇集所有认证页面 handler 共享的依赖与辅助方法。
type Base struct {
	Sessions *scs.SessionManager
	Renderer Renderer
	Settings *application.SettingsService
	Logger   *slog.Logger
}

// NewPage 构造认证页面的基础数据：CSRF 字段、一次性请求 ID、flash 消息与面板时区。
func (b Base) NewPage(r *http.Request, title string) views.Page {
	page := views.Page{Title: title, CSRFField: csrf.TemplateField(r), RequestID: NewRequestID(), Authenticated: true,
		Values: map[string]string{}, FieldErrors: map[string]string{}, Timezone: b.Location(r).String()}
	if b.Sessions != nil {
		message := b.Sessions.PopString(r.Context(), sessionFlashMessage)
		kind := b.Sessions.PopString(r.Context(), sessionFlashKind)
		if message != "" {
			if kind == "" {
				kind = "success"
			}
			page.Flash = &views.Flash{Kind: kind, Message: message}
		}
	}
	return page
}

// Flash 把一次性成功消息存入服务端 session，由下一次 GET 渲染（http.md §General Rules）。
func (b Base) Flash(r *http.Request, kind, message string) {
	if b.Sessions == nil {
		return
	}
	b.Sessions.Put(r.Context(), sessionFlashKind, kind)
	b.Sessions.Put(r.Context(), sessionFlashMessage, message)
}

func (b Base) Actor(r *http.Request) domain.ID {
	if b.Sessions == nil {
		return ""
	}
	return domain.ID(b.Sessions.GetString(r.Context(), webmiddleware.SessionAdministratorID))
}

func (b Base) SessionToken(r *http.Request) string {
	if b.Sessions == nil {
		return ""
	}
	return b.Sessions.Token(r.Context())
}

func (b Base) Location(r *http.Request) *time.Location {
	if b.Settings == nil {
		return time.UTC
	}
	return b.Settings.Location(r.Context())
}

func (b Base) logger() *slog.Logger {
	if b.Logger == nil {
		return slog.Default()
	}
	return b.Logger
}

// Failure 描述一次业务错误在页面上的呈现方式。
type Failure struct {
	Status  int
	Field   string
	Message string
}

// Classify 把领域错误映射为 HTTP 状态、字段与管理员可见文案（http.md §Response Semantics）。
func Classify(err error) Failure {
	var validation *domain.ValidationError
	if errors.As(err, &validation) {
		return Failure{Status: http.StatusUnprocessableEntity, Field: validation.Field, Message: fieldMessage(validation.Field, validation.Message)}
	}
	var conflict *domain.ConflictError
	if errors.As(err, &conflict) {
		return Failure{Status: http.StatusConflict, Message: conflictMessage(conflict.Message)}
	}
	var state *domain.InvalidStateError
	if errors.As(err, &state) {
		return Failure{Status: http.StatusConflict, Message: conflictMessage(state.Message)}
	}
	var notFound *domain.NotFoundError
	if errors.As(err, &notFound) {
		return Failure{Status: http.StatusNotFound, Message: "请求的资源不存在"}
	}
	return Failure{Status: http.StatusInternalServerError, Message: "请求暂时无法完成"}
}

var fieldMessages = map[string]string{
	"display_name":    "显示名称需为 1–64 个字符、不含控制字符",
	"name":            "名称需为 1–64 个字符、不含控制字符",
	"public_host":     "公开地址无效",
	"listen_address":  "监听地址必须是 IP 字面量，例如 0.0.0.0",
	"port_pool_start": "端口池起始必须在 1024 到 65535 之间",
	"port_pool_end":   "端口池结束必须在 1024 到 65535 之间，且不小于起始端口",
	"port":            "端口必须位于端口池内且未被其他用户占用",
	"method":          "加密方式只支持 2022-blake3-aes-128-gcm 与 2022-blake3-aes-256-gcm",
	"network":         "网络能力只能是 tcp、udp 或 tcp_udp",
	"reset_day":       "重置日必须在 1 到 28 之间",
	"limit_bytes":     "配额必须为正整数且不超过 2^62 字节，或勾选“无限制”",
	"quota":           "配额必须为正整数且不超过 2^62 字节，或勾选“无限制”",
	"template_id":     "请选择一个兼容的入站模板",
	"_request_id":     "表单已过期，请刷新页面后重试",
	"_version":        "资源版本无效，请刷新页面后重试",
}

func fieldMessage(field, fallback string) string {
	if message, ok := fieldMessages[field]; ok {
		return message
	}
	return fallback
}

var conflictMessages = map[string]string{
	"port is already assigned to another user":                        "该端口已分配给其他用户，请更换端口",
	"port pool is exhausted; widen the range on the inbound template": "端口池已耗尽，请在入站模板中扩大端口范围",
	"inbound template changed since the page was loaded":              "入站模板已被修改，页面显示的是最新状态，请核对后重新提交",
	"user name already exists":                                        "显示名称已被使用，请更换后重试",
	"inbound template already exists":                                 "入站模板名称已存在",
	"request identifier was reused with different input":              "该请求已提交过且内容不同，请刷新页面后重新操作",

	"inbound template changed or still has managed users":     "入站模板仍有受管用户或已被修改",
	"inbound template is not compatible":                      "所选入站模板当前不兼容，不能创建用户",
	"connection information is unavailable for deleted users": "已删除用户不再提供连接信息",
	"template still has users, unconfirmed removals or pending synchronization; method and listen address cannot change until they are confirmed absent": "该模板下仍有用户、未确认的移除或待同步操作，加密方式与监听地址暂时不能修改",
}

func conflictMessage(message string) string {
	if translated, ok := conflictMessages[message]; ok {
		return translated
	}
	return "当前状态不允许该操作：" + message
}

// Fail 渲染非表单错误（404/409/500），500 只暴露安全错误编号。
func (b Base) Fail(w http.ResponseWriter, r *http.Request, err error) {
	failure := Classify(err)
	if failure.Status == http.StatusInternalServerError {
		id := NewRequestID()
		b.logger().Error("request failed", logging.FieldRequestID, id, logging.FieldErrorKind, "internal", "path", r.URL.Path)
		b.Renderer.Error(w, failure.Status, failure.Message, id)
		return
	}
	b.Renderer.Error(w, failure.Status, failure.Message, "")
}

func pathID(r *http.Request, name string) (domain.ID, bool) {
	id := domain.ID(r.PathValue(name))
	return id, id.Valid()
}
