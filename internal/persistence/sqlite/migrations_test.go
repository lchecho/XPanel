package sqlite

import (
	"context"
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
