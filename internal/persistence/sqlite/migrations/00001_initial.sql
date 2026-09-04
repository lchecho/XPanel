-- +goose Up
CREATE TABLE panel_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    quota_timezone TEXT NOT NULL,
    key_verifier BLOB NOT NULL,
    key_verifier_nonce BLOB NOT NULL,
    key_encryption_version INTEGER NOT NULL CHECK (key_encryption_version = 1),
    revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE administrators (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    password_version INTEGER NOT NULL DEFAULT 1 CHECK (password_version > 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE admin_sessions (
    id TEXT PRIMARY KEY,
    administrator_id TEXT NOT NULL REFERENCES administrators(id),
    token_digest BLOB NOT NULL UNIQUE,
    password_version INTEGER NOT NULL CHECK (password_version > 0),
    data BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    idle_expires_at INTEGER NOT NULL,
    absolute_expires_at INTEGER NOT NULL,
    revoked_at INTEGER
) STRICT;
CREATE INDEX admin_sessions_administrator_idx ON admin_sessions(administrator_id, revoked_at);

CREATE TABLE managed_xray_instances (
    id TEXT PRIMARY KEY,
    singleton INTEGER NOT NULL DEFAULT 1 UNIQUE CHECK (singleton = 1),
    name TEXT NOT NULL,
    api_endpoint TEXT NOT NULL,
    supported_runtime_version TEXT NOT NULL,
    health_state TEXT NOT NULL DEFAULT 'unknown' CHECK (health_state IN ('unknown','healthy','unreachable','incompatible')),
    boot_epoch TEXT,
    last_success_at INTEGER,
    last_error_code TEXT,
    last_error_summary TEXT,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE access_profiles (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES managed_xray_instances(id),
    name TEXT NOT NULL,
    normalized_name TEXT NOT NULL,
    inbound_tag TEXT NOT NULL,
    public_host TEXT NOT NULL,
    public_port INTEGER NOT NULL CHECK (public_port BETWEEN 1 AND 65535),
    method TEXT NOT NULL CHECK (method IN ('2022-blake3-aes-128-gcm','2022-blake3-aes-256-gcm')),
    network TEXT NOT NULL CHECK (network IN ('tcp','udp','tcp_udp')),
    server_key_ciphertext BLOB NOT NULL,
    server_key_nonce BLOB NOT NULL,
    key_encryption_version INTEGER NOT NULL CHECK (key_encryption_version > 0),
    bootstrap_statistics_id TEXT NOT NULL,
    compatibility_state TEXT NOT NULL CHECK (compatibility_state IN ('unverified','compatible','incompatible','unreachable')),
    compatibility_reason TEXT,
    last_validated_at INTEGER,
    revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    archived_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(instance_id, inbound_tag)
) STRICT;
CREATE UNIQUE INDEX access_profiles_active_name_idx ON access_profiles(normalized_name) WHERE archived_at IS NULL;

CREATE TABLE xray_user_identities (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES managed_xray_instances(id),
    profile_id TEXT NOT NULL REFERENCES access_profiles(id),
    statistics_id TEXT NOT NULL CHECK (length(statistics_id) > 0 AND instr(statistics_id, '>>>') = 0),
    kind TEXT NOT NULL CHECK (kind IN ('bootstrap','managed')),
    created_at INTEGER NOT NULL,
    UNIQUE(instance_id, statistics_id)
) STRICT;

CREATE TABLE managed_users (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    normalized_name TEXT NOT NULL,
    lifecycle_state TEXT NOT NULL CHECK (lifecycle_state IN ('active','deleted')),
    revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    deleted_at INTEGER
) STRICT;
CREATE UNIQUE INDEX managed_users_active_name_idx ON managed_users(normalized_name) WHERE deleted_at IS NULL;

CREATE TABLE access_allocations (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL UNIQUE REFERENCES managed_users(id),
    profile_id TEXT NOT NULL REFERENCES access_profiles(id),
    identity_id TEXT NOT NULL UNIQUE REFERENCES xray_user_identities(id),
    admin_enabled INTEGER NOT NULL CHECK (admin_enabled IN (0,1)),
    quota_state TEXT NOT NULL CHECK (quota_state IN ('within_limit','exceeded')),
    projection_state TEXT NOT NULL CHECK (projection_state IN ('unknown','pending','present','absent','error')),
    observed_present INTEGER CHECK (observed_present IS NULL OR observed_present IN (0,1)),
    desired_revision INTEGER NOT NULL CHECK (desired_revision >= 0),
    synced_revision INTEGER NOT NULL CHECK (synced_revision >= 0),
    desired_credential_version INTEGER NOT NULL CHECK (desired_credential_version > 0),
    synced_credential_version INTEGER CHECK (synced_credential_version > 0),
    last_sync_at INTEGER,
    last_sync_error_code TEXT,
    last_sync_error_summary TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE access_credentials (
    id TEXT PRIMARY KEY,
    allocation_id TEXT NOT NULL REFERENCES access_allocations(id),
    version INTEGER NOT NULL CHECK (version > 0),
    state TEXT NOT NULL CHECK (state IN ('pending','active','retired','destroyed')),
    key_ciphertext BLOB,
    key_nonce BLOB,
    key_encryption_version INTEGER,
    created_at INTEGER NOT NULL,
    activated_at INTEGER,
    retired_at INTEGER,
    UNIQUE(allocation_id, version),
    CHECK ((state = 'destroyed' AND key_ciphertext IS NULL AND key_nonce IS NULL) OR
           (state != 'destroyed' AND key_ciphertext IS NOT NULL AND key_nonce IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX access_credentials_pending_idx ON access_credentials(allocation_id) WHERE state = 'pending';
CREATE UNIQUE INDEX access_credentials_active_idx ON access_credentials(allocation_id) WHERE state = 'active';

CREATE TABLE quota_policies (
    allocation_id TEXT PRIMARY KEY REFERENCES access_allocations(id),
    limit_bytes INTEGER CHECK (limit_bytes IS NULL OR (limit_bytes > 0 AND limit_bytes <= 4611686018427387904)),
    reset_day INTEGER NOT NULL CHECK (reset_day BETWEEN 1 AND 28),
    revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE quota_cycles (
    id TEXT PRIMARY KEY,
    allocation_id TEXT NOT NULL REFERENCES access_allocations(id),
    starts_at_utc INTEGER NOT NULL,
    ends_at_utc INTEGER NOT NULL CHECK (ends_at_utc > starts_at_utc),
    timezone_name TEXT NOT NULL,
    reset_day INTEGER NOT NULL CHECK (reset_day BETWEEN 1 AND 28),
    status TEXT NOT NULL CHECK (status IN ('open','closed')),
    gross_uplink_bytes INTEGER NOT NULL DEFAULT 0 CHECK (gross_uplink_bytes >= 0),
    gross_downlink_bytes INTEGER NOT NULL DEFAULT 0 CHECK (gross_downlink_bytes >= 0),
    accounted_uplink_bytes INTEGER NOT NULL DEFAULT 0 CHECK (accounted_uplink_bytes >= 0),
    accounted_downlink_bytes INTEGER NOT NULL DEFAULT 0 CHECK (accounted_downlink_bytes >= 0),
    manual_reset_count INTEGER NOT NULL DEFAULT 0 CHECK (manual_reset_count >= 0),
    opened_at INTEGER NOT NULL,
    closed_at INTEGER,
    UNIQUE(allocation_id, starts_at_utc)
) STRICT;
CREATE UNIQUE INDEX quota_cycles_open_idx ON quota_cycles(allocation_id) WHERE status = 'open';

CREATE TABLE allocation_traffic_totals (
    allocation_id TEXT PRIMARY KEY REFERENCES access_allocations(id),
    uplink_bytes INTEGER NOT NULL DEFAULT 0 CHECK (uplink_bytes >= 0),
    downlink_bytes INTEGER NOT NULL DEFAULT 0 CHECK (downlink_bytes >= 0),
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE traffic_cursors (
    allocation_id TEXT PRIMARY KEY REFERENCES access_allocations(id),
    boot_epoch TEXT,
    uplink_counter INTEGER CHECK (uplink_counter >= 0),
    downlink_counter INTEGER CHECK (downlink_counter >= 0),
    uplink_epoch INTEGER NOT NULL DEFAULT 0 CHECK (uplink_epoch >= 0),
    downlink_epoch INTEGER NOT NULL DEFAULT 0 CHECK (downlink_epoch >= 0),
    last_observed_at INTEGER,
    last_success_at INTEGER,
    missing_since INTEGER,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE daily_traffic_aggregates (
    allocation_id TEXT NOT NULL REFERENCES access_allocations(id),
    day_start_utc INTEGER NOT NULL,
    local_date TEXT NOT NULL,
    timezone_name TEXT NOT NULL,
    uplink_bytes INTEGER NOT NULL DEFAULT 0 CHECK (uplink_bytes >= 0),
    downlink_bytes INTEGER NOT NULL DEFAULT 0 CHECK (downlink_bytes >= 0),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(allocation_id, day_start_utc)
) STRICT;

CREATE TABLE traffic_continuity_events (
    id TEXT PRIMARY KEY,
    allocation_id TEXT NOT NULL REFERENCES access_allocations(id),
    type TEXT NOT NULL CHECK (type IN ('counter_decrease','missing','reappeared','boundary_gap','node_restart','baseline','overflow')),
    direction TEXT CHECK (direction IS NULL OR direction IN ('uplink','downlink')),
    old_counter INTEGER CHECK (old_counter >= 0),
    new_counter INTEGER CHECK (new_counter >= 0),
    counter_epoch INTEGER CHECK (counter_epoch >= 0),
    occurred_at INTEGER NOT NULL,
    safe_summary TEXT NOT NULL
) STRICT;

CREATE TABLE domain_commands (
    id TEXT PRIMARY KEY,
    actor_type TEXT NOT NULL CHECK (actor_type IN ('administrator','local_cli','system')),
    actor_id TEXT,
    command_type TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    request_fingerprint BLOB NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('accepted','completed','failed')),
    result_reference TEXT,
    created_at INTEGER NOT NULL,
    completed_at INTEGER
) STRICT;

CREATE TABLE synchronization_operations (
    id TEXT PRIMARY KEY,
    allocation_id TEXT NOT NULL REFERENCES access_allocations(id),
    desired_revision INTEGER NOT NULL CHECK (desired_revision >= 0),
    desired_presence INTEGER NOT NULL CHECK (desired_presence IN (0,1)),
    desired_credential_version INTEGER CHECK (desired_credential_version > 0),
    reason TEXT NOT NULL CHECK (reason IN ('create','enable','disable','quota_block','quota_restore','rotate','delete','reconcile')),
    phase TEXT NOT NULL CHECK (phase IN ('remove_old','add_desired','confirm','done')),
    state TEXT NOT NULL CHECK (state IN ('pending','leased','retry_wait','succeeded','superseded','permanent_failed')),
    idempotency_key TEXT NOT NULL UNIQUE,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at INTEGER NOT NULL,
    lease_owner TEXT,
    lease_expires_at INTEGER,
    last_error_code TEXT,
    last_error_summary TEXT,
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    completed_at INTEGER,
    UNIQUE(allocation_id, desired_revision)
) STRICT;

CREATE TABLE quota_reset_events (
    id TEXT PRIMARY KEY,
    allocation_id TEXT NOT NULL REFERENCES access_allocations(id),
    quota_cycle_id TEXT NOT NULL REFERENCES quota_cycles(id),
    command_id TEXT NOT NULL UNIQUE REFERENCES domain_commands(id),
    previous_accounted_uplink INTEGER NOT NULL CHECK (previous_accounted_uplink >= 0),
    previous_accounted_downlink INTEGER NOT NULL CHECK (previous_accounted_downlink >= 0),
    uplink_cursor INTEGER CHECK (uplink_cursor >= 0),
    downlink_cursor INTEGER CHECK (downlink_cursor >= 0),
    actor_id TEXT NOT NULL REFERENCES administrators(id),
    occurred_at INTEGER NOT NULL
) STRICT;

CREATE TABLE audit_events (
    id TEXT PRIMARY KEY,
    occurred_at INTEGER NOT NULL,
    actor_type TEXT NOT NULL CHECK (actor_type IN ('administrator','local_cli','system')),
    actor_id TEXT,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    action TEXT NOT NULL,
    result TEXT NOT NULL CHECK (result IN ('accepted','succeeded','failed','superseded')),
    command_id TEXT,
    operation_id TEXT,
    safe_summary TEXT
) STRICT;
CREATE INDEX audit_events_time_idx ON audit_events(occurred_at DESC, id DESC);
CREATE INDEX audit_events_target_idx ON audit_events(target_type, target_id, occurred_at DESC);

-- +goose Down
DROP TABLE audit_events;
DROP TABLE quota_reset_events;
DROP TABLE synchronization_operations;
DROP TABLE domain_commands;
DROP TABLE traffic_continuity_events;
DROP TABLE daily_traffic_aggregates;
DROP TABLE traffic_cursors;
DROP TABLE allocation_traffic_totals;
DROP TABLE quota_cycles;
DROP TABLE quota_policies;
DROP TABLE access_credentials;
DROP TABLE access_allocations;
DROP TABLE managed_users;
DROP TABLE xray_user_identities;
DROP TABLE access_profiles;
DROP TABLE managed_xray_instances;
DROP TABLE admin_sessions;
DROP TABLE administrators;
DROP TABLE panel_settings;
