package logging

import (
	"context"
	"io"
	"log/slog"
)

const (
	FieldComponent    = "component"
	FieldRequestID    = "request_id"
	FieldOperationID  = "operation_id"
	FieldAllocationID = "allocation_id"
	FieldNodeID       = "node_id"
	FieldTargetState  = "target_state"
	FieldResult       = "result"
	FieldDurationMS   = "duration_ms"
	FieldErrorKind    = "error_kind"
	// FieldPort / FieldInboundTag 用于专属入站的生命周期日志；两者都不含任何密钥材料。
	FieldPort       = "port"
	FieldInboundTag = "inbound_tag"
)

func New(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(&redactingHandler{next: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})})
}

type redactingHandler struct{ next slog.Handler }

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, RedactText(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(redactAttr(attr))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		clean[i] = redactAttr(attr)
	}
	return &redactingHandler{next: h.next.WithAttrs(clean)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name)}
}

func redactAttr(attr slog.Attr) slog.Attr {
	if IsSensitiveKey(attr.Key) {
		return slog.String(attr.Key, "[REDACTED]")
	}
	if attr.Value.Kind() == slog.KindGroup {
		group := attr.Value.Group()
		for i := range group {
			group[i] = redactAttr(group[i])
		}
		return slog.Group(attr.Key, attrsToAny(group)...)
	}
	if attr.Value.Kind() == slog.KindString {
		return slog.String(attr.Key, RedactText(attr.Value.String()))
	}
	return attr
}

func attrsToAny(attrs []slog.Attr) []any {
	values := make([]any, len(attrs))
	for i := range attrs {
		values[i] = attrs[i]
	}
	return values
}
