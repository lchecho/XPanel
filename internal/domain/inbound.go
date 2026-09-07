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
// 与 AccessAllocation.DesiredPresent 保持同一真值表：三者全部成立才监听。
func InboundDesiredPresent(lifecycle LifecycleState, adminEnabled bool, quota QuotaState) bool {
	return lifecycle == LifecycleActive && adminEnabled && quota == QuotaWithinLimit
}

// InboundClientCount 是每条专属入站允许的受管客户端数量。
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
