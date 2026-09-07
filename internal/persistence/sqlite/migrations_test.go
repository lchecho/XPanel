package sqlite

import (
	"context"
	"database/sql"
	"testing"
)

func TestMigrationsAreIdempotentAndConstraintsApply(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db.Write); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db.Write); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write.ExecContext(ctx, `INSERT INTO admin_sessions
        (id,administrator_id,token_digest,password_version,data,created_at,last_seen_at,idle_expires_at,absolute_expires_at)
        VALUES ('s','missing',x'01',1,x'01',1,1,2,3)`); err == nil {
		t.Fatal("foreign key constraint did not apply")
	}
}

func TestOnlyOneOpenQuotaCycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db.Write); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`INSERT INTO managed_xray_instances(id,singleton,name,api_endpoint,supported_runtime_version,health_state,updated_at) VALUES ('i',1,'i','127.0.0.1:1','v26.3.27','unknown',1)`,
		`INSERT INTO inbound_templates(id,instance_id,name,normalized_name,public_host,listen_address,port_pool_start,port_pool_end,method,network,compatibility_state,revision,created_at,updated_at) VALUES ('p','i','p','p','host','127.0.0.1',30000,30099,'2022-blake3-aes-256-gcm','tcp','compatible',0,1,1)`,
		`INSERT INTO xray_user_identities(id,instance_id,template_id,statistics_id,kind,created_at) VALUES ('xi','i','p','xpanel-u','managed',1)`,
		`INSERT INTO managed_users(id,display_name,normalized_name,lifecycle_state,revision,created_at,updated_at) VALUES ('u','u','u','active',0,1,1)`,
		`INSERT INTO access_allocations(id,user_id,template_id,identity_id,admin_enabled,quota_state,projection_state,desired_revision,synced_revision,desired_credential_version,created_at,updated_at) VALUES ('a','u','p','xi',1,'within_limit','unknown',1,0,1,1,1)`,
		`INSERT INTO dedicated_inbounds(allocation_id,template_id,inbound_tag,listen_address,port,server_key_ciphertext,server_key_nonce,key_encryption_version,desired_present,created_at,updated_at) VALUES ('a','p','xpanel-a','127.0.0.1',30000,x'01',x'02',1,1,1,1)`,
		`INSERT INTO quota_cycles(id,allocation_id,starts_at_utc,ends_at_utc,timezone_name,reset_day,status,opened_at) VALUES ('c1','a',1,2,'UTC',1,'open',1)`,
	}
	for _, statement := range statements {
		if _, err := db.Write.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Write.ExecContext(ctx, `INSERT INTO quota_cycles(id,allocation_id,starts_at_utc,ends_at_utc,timezone_name,reset_day,status,opened_at) VALUES ('c2','a',2,3,'UTC',1,'open',2)`); err == nil {
		t.Fatal("second open cycle was accepted")
	}
}

// T015：迁移 00004 的部分唯一索引是端口唯一性的唯一权威保证（research.md R-002）。
func TestDedicatedInboundPortUniqueness(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db.Write); err != nil {
		t.Fatal(err)
	}
	seed := []string{
		`INSERT INTO managed_xray_instances(id,singleton,name,api_endpoint,supported_runtime_version,health_state,updated_at) VALUES ('i',1,'i','127.0.0.1:1','v26.3.27','unknown',1)`,
		`INSERT INTO inbound_templates(id,instance_id,name,normalized_name,public_host,listen_address,port_pool_start,port_pool_end,method,network,compatibility_state,revision,created_at,updated_at) VALUES ('p','i','p','p','host','127.0.0.1',30000,30099,'2022-blake3-aes-256-gcm','tcp','compatible',0,1,1)`,
		`INSERT INTO xray_user_identities(id,instance_id,template_id,statistics_id,kind,created_at) VALUES ('xi','i','p','xpanel-u','managed',1)`,
		`INSERT INTO xray_user_identities(id,instance_id,template_id,statistics_id,kind,created_at) VALUES ('xi2','i','p','xpanel-u2','managed',1)`,
		`INSERT INTO managed_users(id,display_name,normalized_name,lifecycle_state,revision,created_at,updated_at) VALUES ('u','u','u','active',0,1,1)`,
		`INSERT INTO managed_users(id,display_name,normalized_name,lifecycle_state,revision,created_at,updated_at) VALUES ('u2','u2','u2','active',0,1,1)`,
		`INSERT INTO access_allocations(id,user_id,template_id,identity_id,admin_enabled,quota_state,projection_state,desired_revision,synced_revision,desired_credential_version,created_at,updated_at) VALUES ('a','u','p','xi',1,'within_limit','unknown',1,0,1,1,1)`,
		`INSERT INTO access_allocations(id,user_id,template_id,identity_id,admin_enabled,quota_state,projection_state,desired_revision,synced_revision,desired_credential_version,created_at,updated_at) VALUES ('a2','u2','p','xi2',1,'within_limit','unknown',1,0,1,1,1)`,
		`INSERT INTO dedicated_inbounds(allocation_id,template_id,inbound_tag,listen_address,port,server_key_ciphertext,server_key_nonce,key_encryption_version,desired_present,created_at,updated_at) VALUES ('a','p','xpanel-a','127.0.0.1',30000,x'01',x'02',1,1,1,1)`,
	}
	for _, statement := range seed {
		if _, err := db.Write.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	duplicate := `INSERT INTO dedicated_inbounds(allocation_id,template_id,inbound_tag,listen_address,port,server_key_ciphertext,server_key_nonce,key_encryption_version,desired_present,created_at,updated_at) VALUES ('a2','p','xpanel-a2','127.0.0.1',30000,x'01',x'02',1,1,1,1)`
	if _, err := db.Write.ExecContext(ctx, duplicate); err == nil {
		t.Fatal("同一监听地址上的重复端口被接受")
	}
	// 释放后同一端口可再次分配。
	if _, err := db.Write.ExecContext(ctx, `UPDATE dedicated_inbounds SET released_at=2 WHERE allocation_id='a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write.ExecContext(ctx, duplicate); err != nil {
		t.Fatalf("端口释放后重新分配失败: %v", err)
	}
	// 不同监听地址上的同一端口互不冲突。
	if _, err := db.Write.ExecContext(ctx, `UPDATE dedicated_inbounds SET listen_address='0.0.0.0' WHERE allocation_id='a'`); err != nil {
		t.Fatal(err)
	}
}

// T015：00004 是破坏性迁移，回滚只恢复结构不恢复数据；up → down → up 必须都能干净跑完，
// 且 down 之后 001 的结构回到位、up 之后 002 的结构再次可用。
func TestPerUserInboundMigrationRollsBackAndForward(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db.Write); err != nil {
		t.Fatal(err)
	}
	if err := migrateDownTo(ctx, db.Write, 3); err != nil {
		t.Fatal(err)
	}
	// 回滚后 001 的结构回来：access_profiles 带服务端密钥列，dedicated_inbounds 不存在。
	if !tableExists(t, db.Write, "access_profiles") || tableExists(t, db.Write, "dedicated_inbounds") ||
		tableExists(t, db.Write, "inbound_templates") {
		t.Fatal("down migration did not restore the 001 schema")
	}
	if !columnExists(t, db.Write, "access_profiles", "server_key_ciphertext") ||
		!columnExists(t, db.Write, "access_allocations", "profile_id") {
		t.Fatal("down migration did not restore the 001 columns")
	}
	if err := Migrate(ctx, db.Write); err != nil {
		t.Fatalf("re-applying 00004 after rollback: %v", err)
	}
	if !tableExists(t, db.Write, "dedicated_inbounds") || !tableExists(t, db.Write, "inbound_templates") ||
		tableExists(t, db.Write, "access_profiles") {
		t.Fatal("re-applied migration did not restore the 002 schema")
	}
	var violations int
	if err := db.Write.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign key violations after the round trip = %d, %v", violations, err)
	}
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count > 0
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count > 0
}

// T094：迁移 00008 必须把升级前通过门禁的模板置回待验证——它们的兼容结论是对「哪个 Xray 进程」
// 做出的已不可考，没有世代证据就不能继续放行新用户。
func TestCapabilityGenerationMigrationInvalidatesPreUpgradeEvidence(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// 先迁到 00007（引入 validated_boot_epoch 的那一版），构造一条「升级前已通过门禁」的模板。
	if err := migrateUpTo(ctx, db.Write, 7); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`INSERT INTO managed_xray_instances(id,singleton,name,api_endpoint,supported_runtime_version,health_state,boot_epoch,updated_at) VALUES ('i',1,'i','127.0.0.1:1','v26.3.27','healthy','2026-09-04T12:00:00Z',1)`,
		`INSERT INTO inbound_templates(id,instance_id,name,normalized_name,public_host,listen_address,port_pool_start,port_pool_end,method,network,compatibility_state,last_validated_at,validated_boot_epoch,revision,created_at,updated_at) VALUES ('t','i','p','p','host','127.0.0.1',30000,30099,'2022-blake3-aes-256-gcm','tcp','compatible',1,'2026-09-04T12:00:00Z',3,1,1)`,
	}
	for _, statement := range statements {
		if _, err := db.Write.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, db.Write); err != nil {
		t.Fatal(err)
	}
	var state string
	var generation sql.NullInt64
	var revision int64
	if err := db.Read.QueryRowContext(ctx, `SELECT compatibility_state,validated_generation,revision FROM inbound_templates WHERE id='t'`).
		Scan(&state, &generation, &revision); err != nil {
		t.Fatal(err)
	}
	if state != "unverified" || generation.Valid || revision != 4 {
		t.Fatalf("pre-upgrade template after migration: state=%s generation=%#v revision=%d", state, generation, revision)
	}
	var instanceGeneration int64
	if err := db.Read.QueryRowContext(ctx, `SELECT capability_generation FROM managed_xray_instances WHERE singleton=1`).
		Scan(&instanceGeneration); err != nil {
		t.Fatal(err)
	}
	if instanceGeneration != 1 {
		t.Fatalf("instance capability generation after migration = %d, want 1", instanceGeneration)
	}
}
