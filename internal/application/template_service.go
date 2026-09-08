package application

import (
	"context"
	"errors"
	"strconv"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// ErrValidationStale 表示验证期间入站模板已被编辑：旧结果被丢弃，最新 revision 会重新验证。
var ErrValidationStale = errors.New("template changed during validation; result discarded")

// TemplateInput 描述入站模板的登记或编辑。模板不再需要管理员提供服务端密钥（FR-004）。
type TemplateInput struct {
	Name             string
	PublicHost       string
	ListenAddress    string
	PortPoolStart    int
	PortPoolEnd      int
	Method           string
	Network          domain.Network
	RequestID        domain.ID
	ExpectedRevision domain.Revision
	ActorID          domain.ID
	// Fingerprint 由 handler 以 session+action+target+规范化载荷生成；为空时由服务按业务字段计算。
	Fingerprint []byte
}

// RevalidateInput 描述手动重新验证：需要当前 revision，幂等排队。
type RevalidateInput struct {
	ID               domain.ID
	ExpectedRevision domain.Revision
	RequestID        domain.ID
	ActorID          domain.ID
	Fingerprint      []byte
}

// 核心函数：TemplateService 管理入站模板的登记、编辑、验证与重新验证。
//
// 职责：校验并持久化模板元数据；用一次性探针入站验证节点能力；把验证结果按模板 revision 条件提交。
// 约束：验证结果、实例健康与验证审计在一个事务中提交；验证期间发生编辑时丢弃旧结果（FR-021）。
// 说明：用户级统计是否开启无法经 API 证实，只能在采集阶段作为健康诊断暴露（research.md R-007 更正记录）。
type TemplateService struct {
	store   ports.Store
	adapter ports.Adapter
	keyring *security.Keyring
	clock   ports.Clock
	target  ports.InstanceTarget
	notify  func(domain.ID)
}

func NewTemplateService(store ports.Store, adapter ports.Adapter, keyring *security.Keyring, clock ports.Clock,
	target ports.InstanceTarget, notify func(domain.ID)) *TemplateService {
	return &TemplateService{store: store, adapter: adapter, keyring: keyring, clock: clock, target: target, notify: notify}
}

func (s *TemplateService) RegisterTemplate(ctx context.Context, input TemplateInput) (domain.ID, error) {
	if !input.RequestID.Valid() {
		return "", &domain.ValidationError{Field: "_request_id", Message: "invalid request identifier"}
	}
	instance, err := s.store.ManagedInstance(ctx)
	if err != nil {
		return "", err
	}
	templateID, err := domain.NewID()
	if err != nil {
		return "", err
	}
	now := s.clock.Now().UTC()
	template, err := domain.NewInboundTemplate(templateID, instance.ID, input.Name, input.PublicHost, input.ListenAddress,
		input.PortPoolStart, input.PortPoolEnd, input.Method, input.Network, now)
	if err != nil {
		return "", err
	}
	// 登记的指纹不得依赖随机生成的新模板 ID，否则同请求重放永远无法命中（FR-021）。
	fingerprint := input.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = domain.Fingerprint(domain.ActionTemplateRegistered, input.Name, input.PublicHost, input.ListenAddress,
			strconv.Itoa(input.PortPoolStart), strconv.Itoa(input.PortPoolEnd), input.Method, string(input.Network))
	}
	command, audit, err := templateCommand(input, templateID, domain.ActionTemplateRegistered, now, fingerprint)
	if err != nil {
		return "", err
	}
	id, replay, err := s.store.CreateTemplate(ctx, ports.TemplateRecord{Template: template}, command, audit)
	if err != nil {
		return "", err
	}
	if !replay && s.notify != nil {
		s.notify(id)
	}
	return id, nil
}

// List 返回未归档的入站模板；compatibleOnly 时只返回可作为新用户目标的模板。
func (s *TemplateService) List(ctx context.Context, compatibleOnly bool) ([]ports.TemplateRecord, error) {
	return s.store.Templates(ctx, compatibleOnly)
}

func (s *TemplateService) Get(ctx context.Context, id domain.ID) (ports.TemplateRecord, error) {
	return s.store.Template(ctx, id)
}

// StatsSuspect 判定某模板是否「已有在监听的用户，却从未读到任何用户级计数」。
//
// 主门禁在模板校验期：探针入站上跑一次认证流量并回读计数器，漏配 policy 的节点根本无法通过校验
// （research.md C-007）。本提示覆盖的是「校验通过之后运维又把 policy 关掉并重启节点」这种后续漂移，
// 它也可能只是还没人用过，因此只作为提示，不改变模板的兼容状态。
func (s *TemplateService) StatsSuspect(ctx context.Context, id domain.ID) (bool, error) {
	listening, observed, err := s.store.TemplateCounterEvidence(ctx, id)
	if err != nil {
		return false, err
	}
	return listening > 0 && observed == 0, nil
}

// PortUsage 返回某模板的端口池占用情况，供界面展示与端口分配判断。
func (s *TemplateService) PortUsage(ctx context.Context, id domain.ID) (ports.PortPoolUsage, error) {
	return s.store.PortPoolUsage(ctx, id)
}

func (s *TemplateService) UpdateTemplate(ctx context.Context, id domain.ID, input TemplateInput) error {
	current, err := s.store.Template(ctx, id)
	if err != nil {
		return err
	}
	validated, err := domain.NewInboundTemplate(id, current.Template.InstanceID, input.Name, input.PublicHost,
		input.ListenAddress, input.PortPoolStart, input.PortPoolEnd, input.Method, input.Network, s.clock.Now())
	if err != nil {
		return err
	}
	validated.Revision = current.Template.Revision
	validated.CreatedAt = current.Template.CreatedAt
	validated.UpdatedAt = s.clock.Now().UTC()
	validated.Compatibility = current.Template.Compatibility
	validated.CompatibilityReason = current.Template.CompatibilityReason
	validated.LastValidatedAt = current.Template.LastValidatedAt
	validated.ArchivedAt = current.Template.ArchivedAt
	// 契约字段：加密方式与监听地址决定已创建入站的密钥长度与绑定地址，变更后必须重新验证。
	contractChanged := current.Template.Method != validated.Method || current.Template.ListenAddress != validated.ListenAddress
	if contractChanged {
		validated.Compatibility = domain.CompatibilityUnverified
		validated.CompatibilityReason = ""
		validated.LastValidatedAt = nil
	}
	fingerprint := input.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = domain.Fingerprint(domain.ActionTemplateUpdated, id.String(), input.Name, input.PublicHost,
			input.ListenAddress, strconv.Itoa(input.PortPoolStart), strconv.Itoa(input.PortPoolEnd), input.Method,
			string(input.Network), strconv.FormatInt(int64(input.ExpectedRevision), 10))
	}
	command, audit, err := templateCommand(input, id, domain.ActionTemplateUpdated, validated.UpdatedAt, fingerprint)
	if err != nil {
		return err
	}
	if err := s.store.UpdateTemplate(ctx, ports.TemplateRecord{Template: validated},
		ports.RevisionMatch{Expected: input.ExpectedRevision}, command, audit); err != nil {
		return err
	}
	if contractChanged && s.notify != nil {
		s.notify(id)
	}
	return nil
}

func templateCommand(input TemplateInput, id domain.ID, action string, now time.Time, fingerprint []byte) (domain.DomainCommand, domain.AuditEvent, error) {
	auditID, err := domain.NewID()
	if err != nil {
		return domain.DomainCommand{}, domain.AuditEvent{}, err
	}
	actor := input.ActorID
	completed := now
	command := domain.DomainCommand{ID: input.RequestID, ActorType: domain.ActorAdministrator, ActorID: &actor,
		CommandType: action, TargetType: "template", TargetID: id, RequestFingerprint: fingerprint,
		State: domain.CommandCompleted, ResultReference: "/templates/" + id.String(), CreatedAt: now, CompletedAt: &completed}
	audit := domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
		TargetType: "template", TargetID: id, Action: action, Result: domain.AuditSucceeded,
		CommandID: &input.RequestID, SafeSummary: "inbound template saved"}
	return command, audit, nil
}

// Revalidate 以 revision 条件把模板置回 unverified 并递增 revision，使验证期间到达的旧结果被丢弃；同请求重放幂等。
func (s *TemplateService) Revalidate(ctx context.Context, input RevalidateInput) (bool, error) {
	if !input.RequestID.Valid() {
		return false, &domain.ValidationError{Field: "_request_id", Message: "invalid request identifier"}
	}
	fingerprint := input.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = domain.Fingerprint(domain.ActionTemplateRevalidationRequested, input.ID.String(), strconv.FormatInt(int64(input.ExpectedRevision), 10))
	}
	now := s.clock.Now().UTC()
	actor := input.ActorID
	completed := now
	auditID, err := domain.NewID()
	if err != nil {
		return false, err
	}
	command := domain.DomainCommand{ID: input.RequestID, ActorType: domain.ActorAdministrator, ActorID: &actor,
		CommandType: domain.ActionTemplateRevalidationRequested, TargetType: "template", TargetID: input.ID, RequestFingerprint: fingerprint,
		State: domain.CommandCompleted, ResultReference: "/templates/" + input.ID.String(), CreatedAt: now, CompletedAt: &completed}
	audit := domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor, TargetType: "template",
		TargetID: input.ID, Action: domain.ActionTemplateRevalidationRequested, Result: domain.AuditAccepted, CommandID: &input.RequestID,
		SafeSummary: "administrator requested template revalidation"}
	replay, err := s.store.RequestRevalidation(ctx, input.ID, input.ExpectedRevision, command, audit)
	if err != nil {
		return false, err
	}
	if !replay && s.notify != nil {
		s.notify(input.ID)
	}
	return replay, nil
}

// RunValidation 用一次性探针入站验证节点能力；结果只在模板 revision 未变化时提交，否则返回 ErrValidationStale。
func (s *TemplateService) RunValidation(ctx context.Context, id domain.ID) error {
	record, err := s.store.Template(ctx, id)
	if err != nil {
		return err
	}
	now := s.clock.Now().UTC()
	observation, err := s.adapter.Probe(ctx, s.target)
	if err != nil {
		return s.complete(ctx, record, domain.CompatibilityUnreachable, adapterSummary(err, "Xray API is unavailable"), nil, now)
	}
	assigned, err := s.store.AssignedPorts(ctx, id)
	if err != nil {
		return err
	}
	probePort, portErr := domain.NextAvailablePort(record.Template.Pool, assigned)
	if portErr != nil {
		// 端口池耗尽时无法执行探针；这不是节点不兼容，标记为待验证并说明原因。
		return s.complete(ctx, record, domain.CompatibilityUnverified,
			"port pool is exhausted; free a port or widen the range before validating", &observation, now)
	}
	capabilities, err := s.adapter.ValidateTemplate(ctx, ports.TemplateProbe{TemplateID: id,
		ListenAddress: record.Template.ListenAddress, ProbePort: probePort, Method: record.Template.Method,
		Network: record.Template.Network})
	if err != nil {
		state := domain.CompatibilityIncompatible
		var adapterErr *ports.AdapterError
		if errors.As(err, &adapterErr) && adapterErr.Retryable {
			state = domain.CompatibilityUnreachable
		}
		return s.complete(ctx, record, state, adapterSummary(err, "template validation failed"), &observation, now)
	}
	if !capabilities.Compatible() {
		reason := capabilities.CompatibilityReason
		if reason == "" {
			reason = "node does not satisfy the SS2022 multi-user contract"
		}
		return s.complete(ctx, record, domain.CompatibilityIncompatible, reason, &observation, now)
	}
	return s.complete(ctx, record, domain.CompatibilityCompatible, "", &observation, now)
}

func adapterSummary(err error, fallback string) string {
	var adapterErr *ports.AdapterError
	if errors.As(err, &adapterErr) && adapterErr.SafeSummary != "" {
		return adapterErr.SafeSummary
	}
	return fallback
}

// complete 组装验证结果并按模板 revision 条件在一个事务中提交 compatibility、实例健康与审计。
func (s *TemplateService) complete(ctx context.Context, record ports.TemplateRecord, state domain.CompatibilityState, reason string,
	observation *ports.InstanceObservation, now time.Time) error {
	outcome := ports.ValidationOutcome{TemplateID: record.Template.ID, ExpectedRevision: record.Template.Revision, State: state,
		Reason: reason, ValidatedAt: now, Health: "incompatible", ErrorCode: "incompatible_profile", ErrorSummary: reason}
	if observation != nil && observation.BootEpochKnown {
		outcome.BootEpoch = observation.BootEpoch.UTC().Format(time.RFC3339)
		// 结论绑定当前能力世代：世代只在确认的重启后前进，因此这次验证「属于」哪个 Xray 进程是明确的。
		generation, _, err := s.store.AdvanceCapabilityGeneration(ctx, ports.CapabilitySignal{
			BootEpoch: observation.BootEpoch, UptimeSeconds: observation.UptimeSeconds, Known: true},
			domain.CapabilityGenerationTolerance, now)
		if err != nil {
			return err
		}
		outcome.CapabilityGeneration = generation
	}
	switch state {
	case domain.CompatibilityCompatible:
		success := now
		outcome.Health, outcome.ErrorCode, outcome.ErrorSummary, outcome.SuccessAt = "healthy", "", "", &success
	case domain.CompatibilityUnreachable:
		outcome.Health, outcome.ErrorCode = "unreachable", "instance_unavailable"
	case domain.CompatibilityUnverified:
		outcome.Health, outcome.ErrorCode = "healthy", ""
	}
	auditID, err := domain.NewID()
	if err != nil {
		return err
	}
	result := domain.AuditSucceeded
	if state != domain.CompatibilityCompatible {
		result = domain.AuditFailed
	}
	outcome.Audit = domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorSystem, TargetType: "template",
		TargetID: record.Template.ID, Action: domain.ActionTemplateValidated, Result: result, SafeSummary: reason}
	applied, err := s.store.CompleteTemplateValidation(ctx, outcome)
	if err != nil {
		return err
	}
	if !applied {
		return ErrValidationStale
	}
	return nil
}
