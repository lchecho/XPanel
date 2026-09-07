package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// 核心函数：专属入站与端口分配的持久化。
//
// 职责：写入/释放端口分配、读取入站期望与实际状态、统计端口池占用；不负责与 Xray 通信。
// 约束：端口唯一性由部分唯一索引 dedicated_inbounds_port_idx 保证，应用层不得以查询代替约束
//
//	（Xray 不拒绝重复端口，见 research.md R-002）。
//
// AI-LOCK：唯一性冲突必须冒泡为 ConflictError，不得吞掉后重试其他端口——那会让指定端口的请求静默改写。

// insertDedicatedInbound 在创建用户的同一事务内写入专属入站与端口分配。
func insertDedicatedInbound(ctx context.Context, tx *sql.Tx, record ports.InboundRecord) error {
	i := record.Inbound
	if err := domain.ValidateInboundTag(i.InboundTag); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO dedicated_inbounds
        (allocation_id,template_id,inbound_tag,listen_address,port,server_key_ciphertext,server_key_nonce,
         key_encryption_version,desired_present,observed_present,last_sync_at,released_at,created_at,updated_at)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, i.AllocationID.String(), i.TemplateID.String(), i.InboundTag,
		i.ListenAddress, i.Port, record.ServerKeyCiphertext, record.ServerKeyNonce, record.KeyEncryptionVersion,
		boolInt(i.DesiredPresent), nullableBool(i.ObservedPresent), nullTime(i.LastSyncAt), nullTime(i.ReleasedAt),
		millis(i.CreatedAt), millis(i.UpdatedAt))
	if err != nil {
		return translateConstraint(err, "port is already assigned to another user")
	}
	return nil
}

// setInboundDesired 更新入站的期望监听状态，供决策路径在事务内调用。
func setInboundDesired(ctx context.Context, tx *sql.Tx, allocationID string, desired bool, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE dedicated_inbounds SET desired_present=?,updated_at=? WHERE allocation_id=? AND released_at IS NULL`,
		boolInt(desired), millis(now), allocationID)
	return err
}

// releaseInboundPort 在删除确认事务内释放端口；历史行保留供审计（data-model.md §事务边界 2）。
func releaseInboundPort(ctx context.Context, tx *sql.Tx, allocationID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE dedicated_inbounds SET released_at=?,desired_present=0,observed_present=0,updated_at=?
        WHERE allocation_id=? AND released_at IS NULL`, millis(now), millis(now), allocationID)
	return err
}

// AssignedPorts 返回与该模板同一监听地址上仍被占用的端口。唯一性以监听地址为界，
// 因为 Xray 的绑定单位是 (listen, port)，两个模板可能共用同一监听地址。
func (s *Store) AssignedPorts(ctx context.Context, templateID domain.ID) ([]int, error) {
	rows, err := s.db.Read.QueryContext(ctx, `SELECT d.port FROM dedicated_inbounds d
        WHERE d.released_at IS NULL AND d.listen_address=(SELECT listen_address FROM inbound_templates WHERE id=?)
        ORDER BY d.port`, templateID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []int
	for rows.Next() {
		var port int
		if err := rows.Scan(&port); err != nil {
			return nil, err
		}
		result = append(result, port)
	}
	return result, rows.Err()
}

const inboundSelect = `SELECT allocation_id,template_id,inbound_tag,listen_address,port,desired_present,
    observed_present,last_sync_at,released_at,created_at,updated_at FROM dedicated_inbounds`

func scanInbound(row scanner) (domain.DedicatedInbound, error) {
	var inbound domain.DedicatedInbound
	var allocationID, templateID string
	var desired, created, updated int64
	var observed, lastSync, released sql.NullInt64
	if err := row.Scan(&allocationID, &templateID, &inbound.InboundTag, &inbound.ListenAddress, &inbound.Port,
		&desired, &observed, &lastSync, &released, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return inbound, &domain.NotFoundError{Resource: "dedicated inbound"}
		}
		return inbound, err
	}
	inbound.AllocationID, inbound.TemplateID = domain.ID(allocationID), domain.ID(templateID)
	inbound.DesiredPresent = desired != 0
	setBool(&inbound.ObservedPresent, observed)
	setTime(&inbound.LastSyncAt, lastSync)
	setTime(&inbound.ReleasedAt, released)
	inbound.CreatedAt, inbound.UpdatedAt = fromMillis(created), fromMillis(updated)
	return inbound, nil
}

func (s *Store) DedicatedInbound(ctx context.Context, allocationID domain.ID) (domain.DedicatedInbound, error) {
	row := s.db.Read.QueryRowContext(ctx, inboundSelect+` WHERE allocation_id=?`, allocationID.String())
	return scanInbound(row)
}

// PanelInbounds 返回仍持有端口分配的全部专属入站，供协调器重建与漂移比对。
func (s *Store) PanelInbounds(ctx context.Context) ([]domain.DedicatedInbound, error) {
	rows, err := s.db.Read.QueryContext(ctx, inboundSelect+` WHERE released_at IS NULL ORDER BY listen_address,port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.DedicatedInbound
	for rows.Next() {
		inbound, err := scanInbound(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, inbound)
	}
	return result, rows.Err()
}

// ConfirmInboundPresence 记录一次已确认的实际监听状态。
func (s *Store) ConfirmInboundPresence(ctx context.Context, allocationID domain.ID, present bool, now time.Time) error {
	_, err := s.db.Write.ExecContext(ctx, `UPDATE dedicated_inbounds SET observed_present=?,last_sync_at=?,updated_at=?
        WHERE allocation_id=? AND released_at IS NULL`, boolInt(present), millis(now), millis(now), allocationID.String())
	return err
}

// confirmInboundPresence 是同一语义的事务内版本，供同步确认事务使用（已释放端口的历史行不再更新）。
func confirmInboundPresence(ctx context.Context, tx *sql.Tx, allocationID string, present bool, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE dedicated_inbounds SET observed_present=?,last_sync_at=?,updated_at=?
        WHERE allocation_id=? AND released_at IS NULL`, boolInt(present), millis(now), millis(now), allocationID)
	return err
}

// PortPoolUsage 汇总某模板的端口池占用情况；Outside 为落在池外的既有分配（缩小端口池后可能出现）。
func (s *Store) PortPoolUsage(ctx context.Context, templateID domain.ID) (ports.PortPoolUsage, error) {
	var usage ports.PortPoolUsage
	var start, end int
	if err := s.db.Read.QueryRowContext(ctx, `SELECT port_pool_start,port_pool_end FROM inbound_templates WHERE id=?`,
		templateID.String()).Scan(&start, &end); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return usage, &domain.NotFoundError{Resource: "inbound template"}
		}
		return usage, err
	}
	pool := domain.PortPool{Start: start, End: end}
	assigned, err := s.AssignedPorts(ctx, templateID)
	if err != nil {
		return usage, err
	}
	usage.Capacity = pool.Capacity()
	usage.Outside = domain.OutsidePoolPorts(pool, assigned)
	for _, port := range assigned {
		if pool.Contains(port) {
			usage.Assigned++
		}
	}
	usage.Remaining = usage.Capacity - usage.Assigned
	if usage.Remaining < 0 {
		usage.Remaining = 0
	}
	return usage, nil
}
