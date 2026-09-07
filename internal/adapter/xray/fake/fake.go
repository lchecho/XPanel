package fake

import (
	"context"
	"sync"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// 测试替身：手写 Xray fake，行为对齐 specs/002-per-user-inbound/research.md 的实测结论。
//
// 约束：入站语义必须忠实复刻真实 Xray——AddInbound 非原子（bind 失败仍注册）、不拒绝重复端口、
// 移除唯一占用者才释放端口、重启清空全部运行时入站。否则测试会掩盖真实故障模式。

type Failure struct {
	Err     error
	Applied bool
}

type Call struct {
	Operation    string
	InboundTag   string
	StatisticsID string
	Port         int
	Reset        bool
}

// Inbound 是 fake 中一条运行时入站。
type Inbound struct {
	Tag           string
	ListenAddress string
	Port          int
	Method        string
	ServerKey     string
}

type Adapter struct {
	mu       sync.Mutex
	Inbounds map[string]*Inbound
	// Users 按入站标签保存客户端；与 Inbounds 同生命周期。
	Users     map[string]map[string]ports.RemoteUser
	Counters  map[string]map[ports.Direction]uint64
	Templates map[string]ports.TemplateCapabilities
	Failures  map[string][]Failure
	Calls     []Call
	Available bool
	BootEpoch time.Time
	Now       func() time.Time
	// ExternalPorts 模拟被面板外进程占用的端口：CreateInbound 会返回 port_unavailable，
	// 但入站仍会被注册（research.md R-003 的非原子行为）。
	ExternalPorts map[int]bool
	// Delay 模拟慢 RPC；调用会等待 Delay 或 ctx 取消。
	Delay time.Duration
	// On* 钩子在对应调用开始时（锁外）触发一次，用于制造并发竞争。
	OnReadTraffic      func()
	OnValidateTemplate func()
	OnAddUser          func()
	OnRemoveUser       func()
	OnCreateInbound    func()
	OnRemoveInbound    func()
}

func New() *Adapter {
	now := time.Now().UTC()
	return &Adapter{Inbounds: make(map[string]*Inbound), Users: make(map[string]map[string]ports.RemoteUser),
		Counters: make(map[string]map[ports.Direction]uint64), Templates: make(map[string]ports.TemplateCapabilities),
		Failures: make(map[string][]Failure), ExternalPorts: make(map[int]bool), Available: true,
		BootEpoch: now, Now: func() time.Time { return time.Now().UTC() }}
}

func (a *Adapter) failure(operation string) (Failure, bool) {
	list := a.Failures[operation]
	if len(list) == 0 {
		return Failure{}, false
	}
	result := list[0]
	a.Failures[operation] = list[1:]
	return result, true
}

func (a *Adapter) unavailable(operation string) error {
	if a.Available {
		return nil
	}
	return &ports.AdapterError{Kind: ports.ErrorInstanceUnavailable, Operation: operation, Retryable: true, SafeSummary: "fake Xray unavailable"}
}

func (a *Adapter) takeHook(hook *func()) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn := *hook
	*hook = nil
	return fn
}

func (a *Adapter) Probe(context.Context, ports.InstanceTarget) (ports.InstanceObservation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "probe"})
	if err := a.unavailable("probe"); err != nil {
		return ports.InstanceObservation{}, err
	}
	now := a.Now()
	return ports.InstanceObservation{ObservedAt: now, BootEpoch: a.BootEpoch, BootEpochKnown: true,
		UptimeSeconds: uptimeSeconds(now, a.BootEpoch)}, nil
}

// ValidateTemplate 以一次性探针入站证明能力；默认返回全兼容，可通过 Templates 覆盖。
func (a *Adapter) ValidateTemplate(ctx context.Context, probe ports.TemplateProbe) (ports.TemplateCapabilities, error) {
	if hook := a.takeHook(&a.OnValidateTemplate); hook != nil {
		hook()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "validate_template", Port: probe.ProbePort})
	if err := a.unavailable("validate_template"); err != nil {
		return ports.TemplateCapabilities{}, err
	}
	if failure, ok := a.failure("validate_template"); ok {
		return ports.TemplateCapabilities{}, failure.Err
	}
	if result, ok := a.Templates[probe.TemplateID.String()]; ok {
		return result, nil
	}
	return ports.TemplateCapabilities{InboundCreatable: true, InboundRemovable: true, ProtocolSupported: true,
		MethodSupported: true, MultiUserSupported: true, TrafficAccounted: true}, nil
}

func (a *Adapter) ListInbounds(_ context.Context) ([]ports.RemoteInbound, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "list_inbounds"})
	if err := a.unavailable("list_inbounds"); err != nil {
		return nil, err
	}
	result := make([]ports.RemoteInbound, 0, len(a.Inbounds))
	for tag := range a.Inbounds {
		result = append(result, ports.RemoteInbound{InboundTag: tag, PanelManaged: domain.IsPanelNamespace(tag),
			UserCount: int64(len(a.Users[tag]))})
	}
	return result, nil
}

// CreateInbound 复刻真实语义：标签重复报错；端口被外部占用时报错但入站仍被注册；
// 入站之间的端口重复不报错（唯一性由面板的数据库约束保证）。
func (a *Adapter) CreateInbound(ctx context.Context, command ports.CreateInboundCommand) (ports.MutationReceipt, error) {
	if hook := a.takeHook(&a.OnCreateInbound); hook != nil {
		hook()
	}
	if err := a.wait(ctx, "create_inbound"); err != nil {
		return ports.MutationReceipt{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "create_inbound", InboundTag: command.InboundTag,
		StatisticsID: command.Client.StatisticsID, Port: command.Port})
	if err := a.unavailable("create_inbound"); err != nil {
		return ports.MutationReceipt{}, err
	}
	if _, exists := a.Inbounds[command.InboundTag]; exists {
		return ports.MutationReceipt{}, &ports.AdapterError{Kind: ports.ErrorInboundAlreadyExists, Operation: "create_inbound",
			Retryable: false, SafeSummary: "inbound tag already exists"}
	}
	failure, fails := a.failure("create_inbound")
	external := a.ExternalPorts[command.Port]
	if !fails || failure.Applied || external {
		// 注册入站。外部占用时同样注册，以复刻 AddInbound 的非原子行为。
		a.Inbounds[command.InboundTag] = &Inbound{Tag: command.InboundTag, ListenAddress: command.ListenAddress,
			Port: command.Port, Method: command.Method, ServerKey: command.ServerKey.Reveal()}
		a.Users[command.InboundTag] = map[string]ports.RemoteUser{command.Client.StatisticsID: {
			StatisticsID: command.Client.StatisticsID, CredentialVersion: command.Client.CredentialVersion,
			Present: true, Kind: "managed"}}
	}
	if external {
		return ports.MutationReceipt{}, &ports.AdapterError{Kind: ports.ErrorPortUnavailable, Operation: "create_inbound",
			Retryable: false, SafeSummary: "port is already in use on the node"}
	}
	if fails {
		return ports.MutationReceipt{}, failure.Err
	}
	return ports.MutationReceipt{OperationID: command.OperationID, ObservedAt: a.Now()}, nil
}

func (a *Adapter) RemoveInbound(ctx context.Context, command ports.RemoveInboundCommand) (ports.MutationReceipt, error) {
	if hook := a.takeHook(&a.OnRemoveInbound); hook != nil {
		hook()
	}
	if err := a.wait(ctx, "remove_inbound"); err != nil {
		return ports.MutationReceipt{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "remove_inbound", InboundTag: command.InboundTag})
	if err := a.unavailable("remove_inbound"); err != nil {
		return ports.MutationReceipt{}, err
	}
	if !domain.IsPanelNamespace(command.InboundTag) {
		return ports.MutationReceipt{}, &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "remove_inbound",
			Retryable: false, SafeSummary: "refusing to remove an inbound outside the panel namespace"}
	}
	failure, fails := a.failure("remove_inbound")
	_, exists := a.Inbounds[command.InboundTag]
	if !fails || failure.Applied {
		delete(a.Inbounds, command.InboundTag)
		delete(a.Users, command.InboundTag)
	}
	if fails {
		return ports.MutationReceipt{}, failure.Err
	}
	if !exists {
		return ports.MutationReceipt{}, &ports.AdapterError{Kind: ports.ErrorInboundNotFound, Operation: "remove_inbound",
			Retryable: false, SafeSummary: "inbound was not found"}
	}
	return ports.MutationReceipt{OperationID: command.OperationID, ObservedAt: a.Now()}, nil
}

func (a *Adapter) ListUsers(_ context.Context, inbound ports.RuntimeInbound) ([]ports.RemoteUser, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "list_users", InboundTag: inbound.InboundTag})
	if err := a.unavailable("list_users"); err != nil {
		return nil, err
	}
	result := make([]ports.RemoteUser, 0, len(a.Users[inbound.InboundTag]))
	for _, user := range a.Users[inbound.InboundTag] {
		result = append(result, user)
	}
	return result, nil
}

func (a *Adapter) AddUser(ctx context.Context, command ports.AddUserCommand) (ports.MutationReceipt, error) {
	if hook := a.takeHook(&a.OnAddUser); hook != nil {
		hook()
	}
	if err := a.wait(ctx, "add_user"); err != nil {
		return ports.MutationReceipt{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "add_user", InboundTag: command.InboundTag, StatisticsID: command.StatisticsID})
	if err := a.unavailable("add_user"); err != nil {
		return ports.MutationReceipt{}, err
	}
	if err := guardPanelInbound("add_user", command.InboundTag); err != nil {
		return ports.MutationReceipt{}, err
	}
	if _, exists := a.Inbounds[command.InboundTag]; !exists {
		return ports.MutationReceipt{}, &ports.AdapterError{Kind: ports.ErrorInboundNotFound, Operation: "add_user",
			Retryable: false, SafeSummary: "inbound was not found"}
	}
	failure, fails := a.failure("add_user")
	if a.Users[command.InboundTag] == nil {
		a.Users[command.InboundTag] = make(map[string]ports.RemoteUser)
	}
	if !fails || failure.Applied {
		a.Users[command.InboundTag][command.StatisticsID] = ports.RemoteUser{StatisticsID: command.StatisticsID,
			CredentialVersion: command.CredentialVersion, Present: true, Kind: "managed"}
	}
	if fails {
		return ports.MutationReceipt{}, failure.Err
	}
	return ports.MutationReceipt{OperationID: command.OperationID, ObservedAt: a.Now()}, nil
}

func (a *Adapter) RemoveUser(ctx context.Context, command ports.RemoveUserCommand) (ports.MutationReceipt, error) {
	if hook := a.takeHook(&a.OnRemoveUser); hook != nil {
		hook()
	}
	if err := a.wait(ctx, "remove_user"); err != nil {
		return ports.MutationReceipt{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "remove_user", InboundTag: command.InboundTag, StatisticsID: command.StatisticsID})
	if err := a.unavailable("remove_user"); err != nil {
		return ports.MutationReceipt{}, err
	}
	if err := guardPanelInbound("remove_user", command.InboundTag); err != nil {
		return ports.MutationReceipt{}, err
	}
	failure, fails := a.failure("remove_user")
	// 复刻真实适配器的最终防线：移除不得让入站失去最后一个受管客户端（FR-019）。
	if _, exists := a.Inbounds[command.InboundTag]; exists && len(a.Users[command.InboundTag]) <= 1 && !fails {
		if _, target := a.Users[command.InboundTag][command.StatisticsID]; target {
			return ports.MutationReceipt{}, &ports.AdapterError{Kind: ports.ErrorLastManagedClient, Operation: "remove_user",
				Retryable: false, SafeSummary: "refusing to remove the last managed client of an inbound"}
		}
	}
	if _, exists := a.Inbounds[command.InboundTag]; !exists && !fails {
		// 入站整体不在时，真实 Xray 的 AlterInbound 报的是「找不到该入站」而不是「找不到用户」。
		return ports.MutationReceipt{}, &ports.AdapterError{Kind: ports.ErrorInboundNotFound, Operation: "remove_user",
			Retryable: false, SafeSummary: "inbound was not found"}
	}
	_, exists := a.Users[command.InboundTag][command.StatisticsID]
	if !fails || failure.Applied {
		delete(a.Users[command.InboundTag], command.StatisticsID)
	}
	if fails {
		return ports.MutationReceipt{}, failure.Err
	}
	if !exists {
		return ports.MutationReceipt{}, &ports.AdapterError{Kind: ports.ErrorUserNotFound, Operation: "remove_user", Retryable: false, SafeSummary: "user not found"}
	}
	return ports.MutationReceipt{OperationID: command.OperationID, ObservedAt: a.Now()}, nil
}

func (a *Adapter) ReadTraffic(_ context.Context, query ports.TrafficQuery) (ports.TrafficRound, error) {
	if hook := a.takeHook(&a.OnReadTraffic); hook != nil {
		hook()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls = append(a.Calls, Call{Operation: "read_traffic"})
	if err := a.unavailable("read_traffic"); err != nil {
		return ports.TrafficRound{}, err
	}
	now := a.Now()
	result := ports.TrafficRound{Observation: ports.InstanceObservation{ObservedAt: now, BootEpoch: a.BootEpoch, BootEpochKnown: true,
		UptimeSeconds: uptimeSeconds(now, a.BootEpoch)}}
	for _, id := range query.StatisticsIDs {
		for _, direction := range []ports.Direction{ports.Uplink, ports.Downlink} {
			value, found := a.Counters[id][direction]
			result.Snapshots = append(result.Snapshots, ports.CounterSnapshot{StatisticsID: id, Direction: direction,
				Bytes: value, ObservedAt: now, Found: found})
		}
	}
	return result, nil
}

func uptimeSeconds(now, boot time.Time) uint32 {
	if !now.After(boot) {
		return 0
	}
	return uint32(now.Sub(boot).Seconds())
}

// Restart 复刻真实重启：全部运行时入站与其客户端消失，端口释放，计数器清零，boot epoch 前进。
func (a *Adapter) Restart() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Inbounds = make(map[string]*Inbound)
	a.Users = make(map[string]map[string]ports.RemoteUser)
	a.Counters = make(map[string]map[ports.Direction]uint64)
	a.BootEpoch = a.Now()
}

// Listening 判定某端口当前是否被面板入站占用，供测试断言端口是否停止监听。
func (a *Adapter) Listening(port int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, inbound := range a.Inbounds {
		if inbound.Port == port {
			return true
		}
	}
	return false
}

// InboundTags 返回当前全部入站标签，供漂移与隔离断言使用。
func (a *Adapter) InboundTags() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	tags := make([]string, 0, len(a.Inbounds))
	for tag := range a.Inbounds {
		tags = append(tags, tag)
	}
	return tags
}

// guardPanelInbound 复刻真实 Adapter 的命名空间边界：面板不得变更保留前缀之外的入站。
func guardPanelInbound(operation, tag string) error {
	if domain.IsPanelNamespace(tag) {
		return nil
	}
	return &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: operation,
		SafeSummary: "refusing to mutate an inbound outside the panel namespace"}
}

// AddExternalInbound 注入一条面板命名空间之外的入站，用于验证面板只读边界。
func (a *Adapter) AddExternalInbound(tag string, port int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Inbounds[tag] = &Inbound{Tag: tag, Port: port}
	a.Users[tag] = map[string]ports.RemoteUser{"operator": {StatisticsID: "operator", Present: true, Kind: "external"}}
}

// AddOrphanInbound 注入一条面板命名空间内但面板无记录的入站，用于漂移清理测试。
func (a *Adapter) AddOrphanInbound(tag string, port int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Inbounds[tag] = &Inbound{Tag: tag, Port: port}
	a.Users[tag] = map[string]ports.RemoteUser{tag + "-client": {StatisticsID: tag + "-client", Present: true, Kind: "managed"}}
}

func (a *Adapter) SetCounter(id string, direction ports.Direction, value uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Counters[id] == nil {
		a.Counters[id] = make(map[ports.Direction]uint64)
	}
	a.Counters[id][direction] = value
}

// wait 在配置了 Delay 时阻塞，ctx 取消则返回可重试的 deadline 错误（变更未应用）。
func (a *Adapter) wait(ctx context.Context, operation string) error {
	a.mu.Lock()
	delay := a.Delay
	a.mu.Unlock()
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return &ports.AdapterError{Kind: ports.ErrorDeadlineExceeded, Operation: operation, Retryable: true, SafeSummary: "fake Xray call cancelled"}
	}
}
