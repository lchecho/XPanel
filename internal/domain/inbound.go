package domain

import (
	"strings"
	"time"
	"unicode"
)

// 核心函数：专属入站的命名空间规则与期望状态派生。
//
// 职责：判定一个 Xray 入站是否归面板管理、生成面板入站标签、由用户与配额事实派生该入站是否应当监听；
// 不负责与 Xray 通信，也不负责端口分配（见 port.go）。
// 约束：面板 MUST 只操作带保留前缀的入站；MUST NOT 存在应当监听却没有受管客户端的入站（宪章 II，FR-019）。
// AI-LOCK：NamespacePrefix 一旦发布不得更改，它是区分面板资源与运维自有入站的唯一依据。

// NamespacePrefix 是面板保留命名空间：入站标签与统计标识共用同一前缀。
const NamespacePrefix = "xpanel-"

// InboundTag 由分配标识派生出稳定且带命名空间的入站标签。
func InboundTag(allocationID ID) string { return NamespacePrefix + allocationID.String() }

// IsPanelNamespace 判定一个入站标签或统计标识是否属于面板保留命名空间。
func IsPanelNamespace(value string) bool { return strings.HasPrefix(value, NamespacePrefix) }

// ValidateInboundTag 校验标签可用于 Xray：带命名空间前缀、非空、无控制字符、不含统计名分隔符。
func ValidateInboundTag(tag string) error {
	if !IsPanelNamespace(tag) {
		return &ValidationError{Field: "inbound_tag", Message: "inbound tag must use the panel namespace prefix"}
	}
	if len(tag) <= len(NamespacePrefix) {
		return &ValidationError{Field: "inbound_tag", Message: "inbound tag must not be the bare namespace prefix"}
	}
	if strings.IndexFunc(tag, unicode.IsControl) >= 0 || strings.Contains(tag, ">>>") {
		return &ValidationError{Field: "inbound_tag", Message: "inbound tag contains forbidden characters"}
	}
	return nil
}

// DedicatedInbound 是面板为单个用户创建并独占的入站在业务侧的表示。
type DedicatedInbound struct {
	AllocationID    ID
	TemplateID      ID
	InboundTag      string
	ListenAddress   string
	Port            int
	DesiredPresent  bool
	ObservedPresent *bool
	LastSyncAt      *time.Time
	ReleasedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Assigned 表示该入站仍持有其端口分配；释放后行仅保留历史，端口回到池中。
func (i DedicatedInbound) Assigned() bool { return i.ReleasedAt == nil }

// InboundDesiredPresent 由用户生命周期、管理员意图和配额状态派生入站是否应当监听。
//
// 职责：这是「该端口是否应当在监听」的唯一真值表，用户、配额与协调三条路径都经
// AccessAllocation.DesiredPresent 走到这里，任何一处新增判定都必须改在此函数内。
// AI-LOCK：三者全部成立才监听；停止访问必须移除整条入站，不得只删客户端（FR-019）。
func InboundDesiredPresent(lifecycle LifecycleState, adminEnabled bool, quota QuotaState) bool {
	return lifecycle == LifecycleActive && adminEnabled && quota == QuotaWithinLimit
}

// RotationSuffix 标记「轮换过渡客户端」：它是面板内部的临时身份，只在一次凭证轮换期间存在。
//
// 存在理由：Xray 不允许原地替换同一 email 的密钥（实测 user_already_exists），只能先删后加；
// 若不先放一个过渡客户端，两次 RPC 之间该入站会短暂没有任何受管客户端，违反 FR-019。
// AI-LOCK：过渡身份的密钥随机生成、只存在于内存、MUST NOT 进入连接信息或任何页面；
// 它的流量计数器不进入任何用户的计量口径。
const RotationSuffix = "-rotate"

// RotationSafetyID 由该用户的统计标识派生出轮换期间的过渡身份，稳定且可识别。
func RotationSafetyID(statisticsID string) string { return statisticsID + RotationSuffix }

// ExpectedIdentityForInbound 由入站标签给出该入站唯一合法的受管身份。
//
// 入站标签与统计标识都由同一个分配标识派生（`xpanel-<allocation>`），因此二者恒等；
// 这条恒等式由 tests/integration 的不变量测试锁定。适配器据此在不依赖数据库的情况下
// 判断「移除之后是否还留有**这条入站真正的**受管客户端」——只认期望身份与它的轮换过渡身份，
// 不能把任意带前缀的身份当成有效后继（T092）。
func ExpectedIdentityForInbound(tag string) string { return tag }

// IsRotationSafetyID 判定某统计标识是否为轮换过渡身份。
func IsRotationSafetyID(value string) bool {
	return IsPanelNamespace(value) && strings.HasSuffix(value, RotationSuffix)
}

// InboundClientCount 是每条专属入站在稳态下的受管客户端数量。
//
// 稳态恰好一个：一个逻辑用户一条入站一个客户端。轮换期间允许短暂存在第二个「过渡客户端」，
// 那是有界的内部状态，不改变稳态约束（FR-017/FR-019）。
//
// AI-LOCK：必须恰好为 1。0 会让 SS2022 入站退化为服务端密钥可直接连接的单用户模式（实测见
// research.md R-005）；>1 会破坏“一个用户一条入站”的隔离前提。
const InboundClientCount = 1

// ValidateInboundClients 校验待创建入站携带的客户端数量满足 InboundClientCount。
func ValidateInboundClients(count int) error {
	if count != InboundClientCount {
		return &InvalidStateError{Message: "a dedicated inbound must carry exactly one managed client"}
	}
	return nil
}
