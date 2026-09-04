# Deployment Configuration Contract

**Format**: UTF-8 JSON  
**Default path**: explicit `--config PATH`; no implicit production location

The JSON file contains runtime settings and secret file paths, not administrator passwords,
Shadowsocks keys, session tokens or a raw application root key.

## Shape

```json
{
  "server": {
    "listen": "127.0.0.1:8080",
    "public_url": "https://panel.example.test",
    "insecure_development": false,
    "read_header_timeout": "5s",
    "request_timeout": "15s",
    "shutdown_timeout": "15s"
  },
  "storage": {
    "database_path": "/var/lib/xpanel/xpanel.db",
    "busy_timeout": "5s"
  },
  "security": {
    "root_key_file": "/etc/xpanel/root.key",
    "session_idle_timeout": "30m",
    "session_absolute_timeout": "12h"
  },
  "xray": {
    "api_endpoint": "127.0.0.1:10085",
    "supported_version": "v26.3.27",
    "rpc_timeout": "5s"
  },
  "workers": {
    "traffic_interval": "5s",
    "reconcile_interval": "15s",
    "max_retry_interval": "30s"
  },
  "initial": {
    "quota_timezone": "UTC"
  },
  "logging": {
    "level": "info"
  }
}
```

## Validation

- Unknown fields are rejected to expose misspellings instead of silently using defaults.
- Durations use Go duration syntax and must be positive.
- `server.listen` and `xray.api_endpoint` must be valid single socket addresses. Plaintext Xray gRPC
  must resolve to loopback; non-loopback startup is rejected.
- `public_url` must be absolute with no user information, query or fragment. Production requires HTTPS.
- `insecure_development=true` is accepted only when the HTTP listener and Xray endpoint are loopback;
  it permits a non-Secure development cookie but must emit a visible startup warning.
- `database_path` must be on local persistent storage. Its directory must not be group/world writable;
  database, WAL, SHM and backup files receive the minimum effective permissions.
- `traffic_interval` defaults to 5 seconds and must not exceed 60 seconds. Values over 5 seconds require
  an explicit startup warning about additional quota overshoot.
- `reconcile_interval` must permit the 60-second recovery target; its maximum is 30 seconds.
- `supported_version` is fixed to the supported baseline. The API does not prove this string; deployment
  validation and the real-process contract suite attest the binary, while runtime probes attest
  capabilities.
- `initial.quota_timezone` must be a valid IANA name and is used only when the singleton panel setting
  is first created. Later changes occur through the authenticated settings flow.

## Root Key File

- Contains standard Base64 encoding of exactly 32 random bytes and a trailing newline only.
- Must be readable only by the XPanel service account; recommended file mode is `0600`, parent directory
  `0700`.
- Must be stored and backed up separately from SQLite. Losing it makes encrypted profile/user keys
  unrecoverable; copying it with a leaked database defeats field encryption.
- The raw key is never logged or loaded into templates.
- Purpose-specific subkeys are derived with HKDF-SHA256 and fixed labels, at minimum
  `xpanel-field-aead-v1` and `xpanel-csrf-v1`; raw key bytes are not reused directly across purposes.
- Field secrets use XChaCha20-Poly1305 with a fresh 24-byte nonce and AAD binding entity, row ID, field
  name and credential/key version.

## Reload and Failure Behavior

- MVP reads configuration only at process start; no hot reload is promised.
- Invalid config, root-key permissions, database PRAGMA verification or migration failure prevents
  workers and the HTTP readiness endpoint from becoming ready.
- Changes to paths, key material, Xray endpoint or worker intervals require a graceful restart.
- Root-key rotation is not an ad-hoc file replacement. It requires a future versioned re-encryption
  operation and is outside this feature's ordinary UI flows.
