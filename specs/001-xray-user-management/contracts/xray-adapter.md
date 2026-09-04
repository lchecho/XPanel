# Xray Adapter Contract

**Runtime**: Xray `v26.3.27` (`d2758a0`)  
**Go module**: `github.com/xtls/xray-core@v1.260327.0`

This is an internal port contract. Domain, application, web and worker packages must not import Xray
protobuf or gRPC status types. The adapter owns type URLs, protobuf accounts, counter names, deadlines
and upstream error translation.

## Conceptual Interface

```go
type Adapter interface {
    Probe(ctx context.Context, target InstanceTarget) (InstanceObservation, error)
    ValidateProfile(ctx context.Context, profile RuntimeProfile) (ProfileCapabilities, error)
    ListUsers(ctx context.Context, profile RuntimeProfile) ([]RemoteUser, error)
    AddUser(ctx context.Context, command AddUserCommand) (MutationReceipt, error)
    RemoveUser(ctx context.Context, command RemoveUserCommand) (MutationReceipt, error)
    ReadTraffic(ctx context.Context, query TrafficQuery) ([]CounterSnapshot, error)
}
```

Exact Go packages and method signatures may refine naming, but must preserve these responsibilities.

## Value Semantics

### InstanceTarget

- Contains instance ID, API endpoint and expected runtime version.
- Plaintext gRPC is permitted only after proving the target is loopback or an equivalent local channel.
- Connection reuse is allowed; every operation still has an explicit context deadline.

### RuntimeProfile

- Contains profile ID, inbound tag, method and bootstrap statistics identity.
- Service key is passed only when needed to validate or compose domain input. Its type must redact
  formatting by default.
- Adapter has no AddInbound, RemoveInbound or config-file editing operation.

### RemoteUser

- Contains stable statistics identity and presence.
- Display names are not remote identities.
- Bootstrap and unknown non-XPanel identities remain distinguishable from the `xpanel-` namespace.

### AddUserCommand

- Contains operation ID, profile tag, allocation ID, globally unique statistics identity, credential
  version and already validated SS2022 user key.
- The adapter encodes `protocol.User{Level: 0, Email: statisticsID}` with
  `shadowsocks_2022.Account{Key: userKey}` inside `AddUserOperation` and `AlterInbound`.
- It never generates, persists or logs the key.

### RemoveUserCommand

- Contains operation ID, profile tag and stable statistics identity.
- It encodes `RemoveUserOperation{Email: statisticsID}`.

### CounterSnapshot

- Contains statistics identity, direction, absolute `uint64` bytes, observation time and `found`.
- `found=false` is not the same as a zero counter.
- Counter names are exact `user>>><id>>>traffic>>>uplink|downlink` values.

### ProfileCapabilities

- Reports inbound presence, protocol/method support, multi-user UserManager support,
  independent uplink/downlink stats, bootstrap visibility and a safe reason.
- Network failure produces `unreachable`; a reachable but incompatible contract produces
  `incompatible`.

## Mutation and Idempotency Rules

- Adapter mutations perform one bounded upstream attempt; persistent retry belongs to the worker.
- `RemoveUser` returning user-not-found is mapped so the coordinator can treat absence as converged.
- `AddUser` returning already-exists never guesses success. The coordinator compares the latest desired
  revision and actual identity, then chooses success or a controlled remove/add repair.
- Unknown, deadline and connection errors after a mutation are uncertain outcomes. The coordinator must
  perform read-after-write before replay.
- `operation_id` is for logs and audit correlation; it does not imply native Xray idempotency support.
- Credential rotation is not exposed as an atomic adapter call. The application persists and drives
  `remove_old -> add_desired -> confirm` phases.
- Xray user sets are changed serially for the single managed instance.

## Traffic Rules

- Reads use StatsService with `reset=false`; adapter never resets Xray counters.
- For at most 20 allocations, the adapter requests exact uplink/downlink counters instead of a broad
  substring query that could include or reset bootstrap/foreign users.
- Missing-before-first-traffic, temporarily unavailable and malformed responses map to distinct stable
  outcomes. No outcome silently overwrites a confirmed SQLite total with zero.
- Manual quota reset is purely a SQLite accounting operation and does not reset Xray statistics.

## Stable Error Kinds

```text
invalid_argument
unsupported_protocol
incompatible_profile
instance_unavailable
deadline_exceeded
profile_not_found
user_already_exists
user_not_found
stats_not_found
version_mismatch
upstream_rejected
internal
```

Each adapter error exposes only `kind`, `operation`, `retryable` and `safe_summary`. The raw gRPC cause
may be retained internally but must be redacted before structured logging. Error strings and attributes
must never include service/user keys, passwords, sessions, full connection URIs or raw protobuf payloads.

## Compatibility Gates

The real-process contract suite for the pinned binary must prove:

1. `xray version`/module agreement and loopback-only management endpoint in the controlled test deployment.
2. Non-empty `settings.clients` with a valid bootstrap user creates SS2022 multi-user mode.
3. Empty `clients`, wrong account type and invalid keys fail as known negative cases.
4. Add/list/remove work with `shadowsocks_2022.Account`; duplicate and missing-user cases map to stable
   errors.
5. A real AES-256 client using `server-key:user-key` passes TCP and UDP traffic and increments both
   exact counters. User-list presence alone is not sufficient proof.
6. Removal rejects new handshakes while an already established connection may continue.
7. Xray restart changes the boot/statistics epoch, drops dynamic users and triggers reconciliation of
   active users only.
8. Timeout after a possibly applied mutation converges through read-after-write without duplicates.
9. Multiple profiles still use globally unique statistics IDs and bootstrap traffic is excluded.

Any Xray runtime or module upgrade must pass this suite before the allowed version changes.
