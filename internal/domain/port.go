package domain

import "sort"

// 核心函数：端口池与端口分配的纯函数。
//
// 职责：描述入站模板可分配的端口区间，并在给定已占用集合时确定性地选出下一个可用端口；
// 不负责持久化，也不保证并发唯一性——唯一性由数据库的部分唯一索引保证（宪章 II，FR-008）。
// AI-LOCK：分配策略必须确定（升序取最小空闲），否则重放同一请求可能得到不同端口。

const (
	// MinAssignablePort 避开特权端口，与 data-model.md 的 CHECK 约束一致。
	MinAssignablePort = 1024
	MaxAssignablePort = 65535
)

// PortPool 是入站模板声明的可分配端口闭区间。
type PortPool struct {
	Start int
	End   int
}

// NewPortPool 构造并校验端口池。
func NewPortPool(start, end int) (PortPool, error) {
	if start < MinAssignablePort || start > MaxAssignablePort {
		return PortPool{}, &ValidationError{Field: "port_pool_start", Message: "port pool start must be between 1024 and 65535"}
	}
	if end < MinAssignablePort || end > MaxAssignablePort {
		return PortPool{}, &ValidationError{Field: "port_pool_end", Message: "port pool end must be between 1024 and 65535"}
	}
	if end < start {
		return PortPool{}, &ValidationError{Field: "port_pool_end", Message: "port pool end must not be smaller than port pool start"}
	}
	return PortPool{Start: start, End: end}, nil
}

// Capacity 是端口池能够容纳的用户数上限。
func (p PortPool) Capacity() int { return p.End - p.Start + 1 }

// Contains 判定端口是否落在池内。缩小端口池后既有分配可能落在池外，此时仍保持可用，只在界面标识。
func (p PortPool) Contains(port int) bool { return port >= p.Start && port <= p.End }

// ErrPortPoolExhausted 表示端口池内已无可分配端口（FR-009）。
var ErrPortPoolExhausted = &ConflictError{Message: "port pool is exhausted; widen the range on the inbound template"}

// NextAvailablePort 在池内按升序返回最小的未占用端口。assigned 可以包含池外端口，它们会被忽略。
func NextAvailablePort(pool PortPool, assigned []int) (int, error) {
	taken := make(map[int]struct{}, len(assigned))
	for _, port := range assigned {
		taken[port] = struct{}{}
	}
	for port := pool.Start; port <= pool.End; port++ {
		if _, used := taken[port]; !used {
			return port, nil
		}
	}
	return 0, ErrPortPoolExhausted
}

// ValidateRequestedPort 校验管理员显式指定的端口：必须在池内且未被占用（FR-007）。
func ValidateRequestedPort(pool PortPool, port int, assigned []int) error {
	if !pool.Contains(port) {
		return &ValidationError{Field: "port", Message: "port is outside the template port pool"}
	}
	for _, used := range assigned {
		if used == port {
			return &ConflictError{Message: "port is already assigned to another user"}
		}
	}
	return nil
}

// OutsidePoolPorts 返回已分配但落在池外的端口，供界面标识（US3 场景 7）。
func OutsidePoolPorts(pool PortPool, assigned []int) []int {
	var outside []int
	for _, port := range assigned {
		if !pool.Contains(port) {
			outside = append(outside, port)
		}
	}
	sort.Ints(outside)
	return outside
}
