# HTTP and HTML Contract

**Audience**: browser administrator and SSR/HTMX handlers  
**Base path**: `/`  
**Content**: UTF-8 HTML; no public JSON CRUD API in MVP

## General Rules

- Full pages and fragments use the same server-side session, authorization, application services and
  validation rules.
- State changes use `POST`; `GET` is safe and must not change business state.
- Forms use `application/x-www-form-urlencoded` and contain `_csrf`, `_request_id`, plus `_version`
  for an existing mutable resource.
- Successful POST requests use `303 See Other` to a canonical GET page. A saved SQLite intent with
  an unavailable Xray is a successful acceptance and redirects to a page visibly marked
  `pending_sync`.
- Every mutable resource exposes a monotonically increasing version. A stale `_version` returns
  `409 Conflict` and displays current state; it never silently overwrites a newer intent.
- `_request_id` is single-use and bound to session, action and target. Repeating the same request and
  payload returns its original canonical result; reusing it with a different payload returns 409.
- IDs in paths are immutable user IDs. Internally, traffic and synchronization use allocation IDs.
- Passwords, service/user keys, full connection URIs, session tokens and CSRF values never appear in
  URLs, flash messages, page titles, logs or audit payloads.
- Success messages travel as a one-time flash stored in the server session and rendered by the next
  GET; they carry a fixed sentence and a user display name at most.
- Display conventions: the UI language is Simplified Chinese without an i18n layer. Byte values use
  IEC binary units (KiB/MiB/GiB/TiB, 1024-based), two decimals from 1 MiB upward, rounded half up.
  Timestamps render in the panel quota timezone as `YYYY-MM-DD HH:MM:SS` with the timezone name in
  the page footer.

## Session and Security Contract

- All management routes except `/login`, `/healthz` and `/readyz` require an authenticated session.
- Production cookie name is `__Host-xpanel_session` with `Secure`, `HttpOnly`, `SameSite=Strict`,
  `Path=/` and no Domain attribute.
- Login rotates the session token. Default idle expiry is 30 minutes and absolute expiry is 12 hours;
  no persistent “remember me” session exists in MVP.
- Logout revokes the current session. CLI password reset increments password version and revokes all
  sessions before reporting success.
- Invalid login messages do not reveal whether a username exists. Five failed attempts for the same
  normalized username and source address in 15 minutes trigger a bounded 429 response; a global
  20-attempt/15-minute guard limits distributed retries. `X-Forwarded-For` is ignored unless a future
  trusted-proxy specification enables it.
- All authenticated HTML responses send `Cache-Control: no-store`, `Referrer-Policy: no-referrer`,
  `X-Content-Type-Options: nosniff` and a CSP equivalent to:

```text
default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'
```

- CSRF token failure returns 403 and performs no business write. Origin protection is defense in
  depth and does not replace the token.
- The CSRF cookie is `__Host-xpanel_csrf` with the same `Secure`, `HttpOnly`, `SameSite=Strict` and
  `Path=/` attributes as the session cookie.
- Unauthenticated fragment requests (`HX-Request: true`) return 403 with a short HTML notice instead
  of a redirect, so a polling fragment never swaps the login page into itself.
- After session expiry the administrator lands on `/` after re-login; unsent form content is not
  preserved. Forms that were open longer than the idle timeout therefore fail with 403 and must be
  re-entered.

## Response Semantics

| Status | Meaning |
|---|---|
| `200` | Successful full page or fragment GET |
| `303` | Successful state-changing command; or safe redirect to login after session expiry |
| `400` | Malformed body, path ID or unsupported content type |
| `401` | Invalid login credentials with a non-enumerating error |
| `403` | Authentication/authorization or CSRF integrity failure |
| `404` | Missing or invisible resource using a non-disclosing page |
| `409` | Resource version conflict, request ID misuse or action invalid for current state |
| `422` | Field-level domain validation failure; non-sensitive fields are preserved |
| `429` | Login throttled; includes a bounded `Retry-After` |
| `500` | Database or unrecoverable internal failure; response exposes only a safe error ID |
| `503` | Readiness failure or temporarily unavailable fragment with visible stale-state text |

All form error pages provide a focusable error summary and field-specific errors. Sensitive inputs
are cleared rather than echoed. Raw SQLite, gRPC, filesystem and stack information never appears.

A 409 page re-renders the form with the administrator's submitted non-sensitive values, shows the
currently stored values alongside, and embeds a fresh `_version` so the administrator can resubmit
without retyping. This is also the multi-tab and multi-device conflict experience.

Stable error kinds and continuity events map to fixed administrator sentences; the safe summary is
shown only as secondary detail:

| Kind / event | Administrator sentence |
|---|---|
| `instance_unavailable`, `deadline_exceeded` | 节点暂时不可达，系统会自动重试 |
| `incompatible_profile`, `unsupported_protocol`, `profile_not_found`, `version_mismatch` | 访问配置与节点不兼容，需要运维检查 Xray 配置 |
| `user_already_exists`, `user_not_found` | 节点状态与面板不一致，系统正在自动修复 |
| `upstream_rejected`, `invalid_argument`, `internal` | 节点拒绝了本次操作，已记录审计 |
| `node_restart` | 节点已重启，已从重启后的计数重新开始计量 |
| `counter_decrease` | 节点计数异常回落，已建立新基线，期间流量可能未计入 |
| `missing` / `reappeared` | 节点暂时未返回该用户的流量计数 / 计数已恢复 |
| `boundary_gap` | 采集间隔超过预期，跨边界流量已归入本次采集完成时的周期 |
| `overflow` | 计数值超出可表示范围，本次样本已忽略 |

## Routes

### Authentication and operations

| Method | Route | Contract |
|---|---|---|
| `GET` | `/login` | Login form; redirects authenticated sessions to `/` |
| `POST` | `/login` | Authenticate and rotate token; 303 to `/` on success |
| `POST` | `/logout` | Revoke current session; 303 to `/login` |
| `GET` | `/healthz` | Process liveness only; no database, Xray or user details |
| `GET` | `/readyz` | 200 only after config validation and migrations; safe summary otherwise |

### Dashboard and fragments

| Method | Route | Contract |
|---|---|---|
| `GET` | `/` | Dashboard with user/status counts, current accounted traffic, node health and sync summary |
| `GET` | `/fragments/dashboard-summary` | Replaceable, authenticated dashboard summary fragment |
| `GET` | `/fragments/users-table` | Replaceable user table using non-sensitive search/status query parameters |

Fragments are polled no faster than every 5 seconds and read SQLite only. They include last confirmed
time, fresh/stale status and pending/error summary. A summary is `stale` when the last successful
collection is older than twice `workers.traffic_interval` (10 s by default) or the last probe failed;
the stale badge sits next to the last-confirmed timestamp and on each affected row, and clears on the
next successful collection. The search and filter form lives outside the swapped fragment; only the
table is replaced. Collection every 5 s plus polling every 5 s bounds display latency at 10 s, which
is the SC-003 target. Every list page renders an explicit empty state: no profiles links to
`/profiles/new`, no compatible profile explains why and links to revalidation, no users links to
`/users/new`, and no audit rows shows a message. `Vary: HX-Request` is required if a handler varies
by that header. A fragment never contains `<html>` or the page layout and never makes a direct Xray
RPC. On replacement it preserves focus; if the focused node disappears, focus moves to a stable result
summary rather than the document body.

### Managed users

| Method | Route | Contract |
|---|---|---|
| `GET` | `/users` | List/search/filter non-deleted users; explicit option can include deleted records |
| `GET` | `/users/new` | Create form with compatible profile and quota policy |
| `POST` | `/users` | Atomically create user, one allocation, credential, policy, cycle and sync intent |
| `GET` | `/users/{user_id}` | Detail, derived state, current accounted/gross usage and daily trend |
| `GET` | `/users/{user_id}/edit` | Editable display name, quota, reset day and admin-enabled intent |
| `POST` | `/users/{user_id}` | Update with `_version`; recalculate quota and desired projection |
| `GET` | `/users/{user_id}/connection` | Explicit no-store page showing connection info only when credential is confirmed |
| `POST` | `/users/{user_id}/enable` | Set admin-enabled intent; quota rules still apply |
| `POST` | `/users/{user_id}/disable` | Clear admin-enabled intent and enqueue removal |
| `GET` | `/users/{user_id}/rotate` | Non-JavaScript rotation confirmation page |
| `POST` | `/users/{user_id}/rotate` | Generate next credential and persist phased rotation intent |
| `GET` | `/users/{user_id}/reset-traffic` | Explain accounted reset and preserved gross history |
| `POST` | `/users/{user_id}/reset-traffic` | Reset current cycle accounted values without changing its end |
| `GET` | `/users/{user_id}/delete` | Non-JavaScript soft-delete confirmation page |
| `POST` | `/users/{user_id}/delete` | Soft delete and enqueue removal; history stays queryable |

Connection information contains public host, port, method, label and a client password assembled from
server and the latest confirmed user key. The page shows the fields individually plus one `ss://` URI
in a read-only text block that can be selected and copied without JavaScript; there is no QR code in
the MVP. The create and edit forms default the reset day to 1 and require an explicit quota choice: an
integer with a unit selector (MiB/GiB/TiB, converted exactly to bytes) or the “无限制” checkbox; an
empty limit without the checkbox is a 422. Rotate and delete confirmation pages must state that old
credentials stop authenticating new connections only after Xray confirms the removal, that established
connections may continue, and that traffic history and audit records are kept. `GET /users/{user_id}`
for a deleted user renders a read-only page without action buttons, and POST actions on a deleted
user return 409. When the allocation's profile is not `compatible`, the detail page and dashboard show
the profile state badge and hide actions that need Xray projection (see data-model §Profile
compatibility). Daily trend is a table of local date, uplink, downlink and total; no charting library.
When active allocations exceed 20, the create form still succeeds but the dashboard and form show a
capacity notice that performance targets are no longer promised. It is unavailable before the first credential is confirmed
or after deletion; disabled and quota-exceeded users may view it with an explicit inactive warning.
During rotation, the page marks connection information unavailable/pending and does not label either
credential usable until the new version is confirmed. The page does not echo either key as a separately
logged field.

Manual traffic reset clears the current cycle's `accounted` values only. Lifetime totals, `gross`
cycle values, daily aggregates and the scheduled cycle boundary remain visible and unchanged. If
quota was the only blocker and admin-enabled remains true, the accepted command also creates a restore
intent.

### Access profiles

| Method | Route | Contract |
|---|---|---|
| `GET` | `/profiles` | List registered profiles and compatibility state |
| `GET` | `/profiles/new` | Registration form for preconfigured SS2022 inbound metadata |
| `POST` | `/profiles` | Persist profile as unverified, then schedule validation outside the transaction |
| `GET` | `/profiles/{profile_id}` | Metadata, health and safe compatibility detail |
| `GET` | `/profiles/{profile_id}/edit` | Edit public metadata; service key field is always blank |
| `POST` | `/profiles/{profile_id}` | Update by version; blank service key keeps the existing secret |
| `POST` | `/profiles/{profile_id}/revalidate` | Schedule a new compatibility probe |

Supported methods are exactly `2022-blake3-aes-128-gcm` and
`2022-blake3-aes-256-gcm`. Profile validation distinguishes `unreachable` from `incompatible`.
Only `compatible` profiles appear as selectable targets for new users. XPanel never creates, removes
or rewrites the inbound itself. XPanel cannot read the inbound's configured password through the API,
so it never verifies that the registered service key matches the preconfigured inbound; the profile
form states this next to the key field, and a wrong key surfaces only as client authentication
failure or in the quickstart traffic check.

### Settings and audit

| Method | Route | Contract |
|---|---|---|
| `GET` | `/settings` | Show the single global quota timezone |
| `POST` | `/settings` | Update valid IANA timezone by version; affects future cycles only |
| `GET` | `/audit` | Cursor-paginated audit list filtered by user, action and result |

Audit output contains actor, time, target, action, result and safe summary. It never exposes passwords,
keys, session/CSRF tokens, complete connection URIs or raw upstream errors. The user filter accepts a
deleted user's ID, and an empty result shows an explicit message rather than a blank table.

## Accessibility and Progressive Enhancement

- Every input has a programmatic label; field errors use `aria-describedby`.
- Status is expressed in text, not color alone. Tables have captions and scoped headers.
- Navigation uses links; actions use buttons. All core routes and confirmation flows are operable by
  keyboard with JavaScript disabled.
- Dynamic summaries use a small `aria-live="polite"` region; whole tables are not repeatedly announced.
- Common mobile widths retain access to all critical actions without requiring horizontal navigation
  to an off-screen control.
- HTMX failures leave the last confirmed content visible and add a visible stale/error summary; they
  never silently imply zero traffic or success.
- These rules are acceptance criteria: the handler-level structure test (T121) verifies them
  automatically, and the quickstart §9 manual pass covers keyboard-only operation and the 360×640 and
  390×844 reference viewports. Screen-reader compatibility beyond the structural rules above is not
  an MVP acceptance criterion.
- `Cache-Control: no-store` also prevents back-forward-cache restoration of authenticated pages. The
  connection page marks the client password with a print-hidden class so printing never emits it.
