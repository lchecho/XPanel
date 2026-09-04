# Quickstart Validation Guide

**Feature**: Xray 多用户管理 MVP  
**Purpose**: implementation-complete end-to-end validation, not a development tutorial

The commands and routes below become runnable after `$speckit-tasks` and implementation. Expected
results are tied to [the feature specification](spec.md), [data model](data-model.md) and
[contracts](contracts/).

## 1. Prerequisites

- Go `1.26.8`.
- Xray binary `v26.3.27` matching module `v1.260327.0`.
- Local persistent directory with mode `0700` for SQLite and the external AEAD master-key file.
- A loopback-only Xray gRPC endpoint and a reachable public test address/port.
- A Shadowsocks 2022 client that supports `2022-blake3-aes-256-gcm` TCP and UDP.
- A disposable environment; quota and restart scenarios intentionally revoke access.

Confirm toolchain and runtime before tests:

```sh
go version
xray version
```

## 2. Prepare the Preconfigured Xray Inbound

Generate distinct 32-byte Base64 keys for the server and permanent bootstrap user:

```sh
openssl rand -base64 32
openssl rand -base64 32
```

Configure Xray v26.3.27 with these required elements. The fixed release uses `clients`; an empty list
is intentionally invalid for this feature.

```json
{
  "api": {
    "listen": "127.0.0.1:10085",
    "services": ["HandlerService", "StatsService"]
  },
  "stats": {},
  "policy": {
    "levels": {
      "0": {
        "statsUserUplink": true,
        "statsUserDownlink": true
      }
    }
  },
  "inbounds": [
    {
      "tag": "ss2022-managed",
      "listen": "0.0.0.0",
      "port": 8388,
      "protocol": "shadowsocks",
      "settings": {
        "method": "2022-blake3-aes-256-gcm",
        "password": "<SERVER_BASE64_KEY>",
        "network": "tcp,udp",
        "clients": [
          {
            "email": "xpanel-bootstrap-reserved",
            "password": "<BOOTSTRAP_BASE64_KEY>"
          }
        ]
      }
    }
  ],
  "outbounds": [{"protocol": "freedom", "tag": "direct"}]
}
```

Start Xray with the test configuration. Do not share the bootstrap key; it is only the permanent marker
that selects multi-user mode and must be excluded from XPanel counts and quotas.

## 3. Build and Initialize XPanel

Run the quality gates:

```sh
go test ./...
go vet ./...
go test -race ./...
```

Build the same single binary used for serving and local administration:

```sh
CGO_ENABLED=0 go build -trimpath -o ./bin/xpanel ./cmd/xpanel
```

Create a root-only AEAD master-key file and application config using the documented deployment sample.
The database and master key must not share backup storage. Initialize the sole administrator:

```sh
./bin/xpanel admin init --config ./xpanel.json
```

The command must prompt without terminal echo, create no default password and print no hash or secret.
Start the service:

```sh
./bin/xpanel serve --config ./xpanel.json
```

Expected startup result:

- migrations finish before `/readyz` becomes ready;
- SQLite reports WAL, foreign keys, busy timeout and synchronous mode as configured;
- Xray version and loopback API checks pass;
- collector and reconciler start only after the database is ready.

## 4. Register and Validate the Access Profile

1. Log in and open `/profiles/new`.
2. Enter public host, port `8388`, inbound tag `ss2022-managed`, AES-256 method, server key and bootstrap
   identity.
3. Save the profile and trigger validation if it is not automatic.

Expected result:

- profile is persisted before the network probe;
- the server key is never echoed on later edit pages;
- state becomes `compatible` only after version, inbound, bootstrap, UserManager and per-user stats
  capabilities are confirmed;
- the bootstrap identity never appears in managed user counts or share pages.

Negative validation: repeat against a disposable Xray config with `clients` omitted or empty. The
profile must become `incompatible`, and creating a user for it must be rejected with a safe reason.

## 5. Create, Share and Meter a User

1. Create one user with the compatible profile, a small positive quota and reset day 1–28.
2. Observe `pending_sync` if Xray is temporarily stopped; start Xray and wait for convergence.
3. Open the explicit connection page after the credential is confirmed.
4. Use the generated client password (`server-key:user-key`) to send TCP and UDP traffic.

Expected result:

- exactly one allocation and one globally unique `xpanel-...` statistics identity are created;
- XPanel generates the user key and submits `shadowsocks_2022.Account`; Xray returns no client config;
- full connection information is assembled only by XPanel and is never logged or audited;
- uplink, downlink, lifetime, daily gross and current accounted values update from persisted SQLite
  data, normally no more than 10 seconds behind;
- no five-second raw sample history is stored.

## 6. Exercise Quota and Manual Reset

1. Send traffic until accounted usage equals or exceeds the small quota.
2. Attempt a new connection after the next collection/sync cycle.
3. Keep an already-established connection active to observe the documented soft-overage behavior.
4. Use the traffic-reset confirmation page.

Expected result:

- at least 95% of normal quota crossings block new connections within 10 seconds; otherwise a clear
  pending/error state appears within 30 seconds;
- an existing connection may continue, and UI wording does not claim hard byte-level enforcement;
- manual reset sets current accounted usage to zero without changing cycle end, lifetime total, gross
  cycle traffic or daily trend;
- if quota was the only blocker, access is restored; manual disable continues to block it.

Also lower a quota below current usage and then raise it above usage or set it unlimited. The first
change must enqueue immediate blocking; the second restores only when no manual/deleted blocker exists.

## 7. Lifecycle, Rotation and Idempotency

For one user, run edit, disable, enable, rotate and delete flows. Submit each POST twice with the same
request ID, then submit a stale resource version.

Expected result:

- duplicate requests resolve to the original result without duplicate users, operations or traffic;
- stale updates return a visible 409 conflict and never overwrite newer intent;
- rotation remains pending through `remove_old -> add_desired -> confirm`; only confirmed connection
  information is presented as current;
- old credentials cannot make new connections after confirmed rotation;
- delete is soft, preserves aggregates/audit, removes access and allows the display name to be reused
  only with a new identity and history.

## 8. Restart and Failure Recovery

Create active, manually disabled, quota-exceeded and deleted examples, then restart Xray. Simulate an
RPC timeout at a point where add/remove may already have applied.

Expected result within 60 seconds of reconnection:

- active users are restored;
- manual-disabled, quota-exceeded and deleted users remain absent;
- bootstrap and unknown non-XPanel identities are untouched;
- boot/statistics epoch changes are recorded without negative or duplicate traffic;
- uncertain mutations use read-after-write and converge without duplicate users;
- stale UI retains the last confirmed totals instead of replacing them with zero.

Restart XPanel between SQLite commit and Xray result confirmation. Persistent synchronization operations
must resume from their lease/revision state and reach the same outcome.

## 9. Authentication and Local Recovery

Validate login, logout, idle/absolute expiry, CSRF failure, duplicate form submission and security
headers. No protected page or fragment may be visible without a valid session.

Reset the administrator password locally:

```sh
./bin/xpanel admin reset-password --config ./xpanel.json
```

Expected result:

- input is hidden and confirmed twice;
- old password and every existing session fail immediately after commit;
- the reset completes within five minutes for an authorized host operator;
- the audit event identifies `local_cli` but contains no password, hash or token;
- no web, email or SMS recovery route exists.

## 10. Release Gate

Before a release, retain evidence that:

- all unit, integration, handler, race and fixed-Xray contract suites passed;
- SQLite backup and restore were exercised together with availability of the separate AEAD master key;
- a restored database reconciled active users without reviving blocked/deleted users;
- both supported AES methods passed valid/invalid key cases, and AES-256 passed real TCP/UDP traffic;
- authenticated pages and audit/log output contain no reusable credentials;
- SC-001 through SC-011 measurements meet the specification thresholds.
