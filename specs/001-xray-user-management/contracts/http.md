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
time, fresh/stale status and pending/error summary. `Vary: HX-Request` is required if a handler varies
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
server and the latest confirmed user key. It is unavailable before the first credential is confirmed
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
or rewrites the inbound itself.

### Settings and audit

| Method | Route | Contract |
|---|---|---|
| `GET` | `/settings` | Show the single global quota timezone |
| `POST` | `/settings` | Update valid IANA timezone by version; affects future cycles only |
| `GET` | `/audit` | Cursor-paginated audit list filtered by user, action and result |

Audit output contains actor, time, target, action, result and safe summary. It never exposes passwords,
keys, session/CSRF tokens, complete connection URIs or raw upstream errors.

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
