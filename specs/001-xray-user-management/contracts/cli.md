# Local CLI Contract

**Binary**: `xpanel`  
**Authorization boundary**: local host access and database/master-key file permissions

## Commands

```text
xpanel serve [--config PATH]
xpanel admin init [--config PATH] [--password-stdin]
xpanel admin reset-password [--config PATH] [--password-stdin]
```

There is no `--password VALUE` option. Passwords are not accepted from environment variables.

## Shared Rules

- `--config` selects the same JSON application configuration used by `serve`; commands do not define a
  second database path or secret-loading mechanism.
- Configuration, database and external AEAD master key must pass the same permission and compatibility
  checks as server startup.
- stdout contains only a concise success result. Safe diagnostics go to stderr.
- No command prints passwords, hashes, service/user keys, session tokens, complete connection URIs or
  raw database values.
- A busy or failed database transaction produces no partial administrator/session/audit update.

## `xpanel admin init`

- Succeeds only if no administrator exists.
- Prompts for username when not supplied through the protected setup input, then prompts twice for the
  password with terminal echo disabled.
- `--password-stdin` is an explicit automation mode. It consumes only stdin, never repeats the value,
  and fails if the input is empty or malformed.
- Applies the same username normalization, password policy and Argon2id service used by web login
  (policy defined in `config.md` §Administrator Credential Policy).
- Creates the administrator and `administrator_initialized` audit event in one transaction.
- If an administrator already exists, exits without modifying it and instructs the operator to use
  `reset-password`.

## `xpanel admin reset-password`

- Does not require the old password; possession of authorized host access is the recovery credential.
- Default TTY mode prompts twice with echo disabled and rejects a mismatch before opening a write
  transaction.
- `--password-stdin` follows the same non-echoing automation rule as initialization.
- In one transaction it replaces the Argon2id hash, increments `password_version`, revokes all admin
  sessions and appends an audit event with `actor_type=local_cli`.
- Success means the old password and every prior browser session are unusable. A new login is required.
- It never mutates users, profiles, quota, Xray projection or traffic records.

## Exit Codes

| Code | Meaning |
|---|---|
| `0` | Command completed and committed |
| `2` | Invalid command arguments, password input or TTY mismatch |
| `3` | Configuration, permission, master-key, migration or database compatibility failure |
| `4` | Business command rejected or transaction failed |

Interrupting input before commit returns a non-zero code and performs no change.
