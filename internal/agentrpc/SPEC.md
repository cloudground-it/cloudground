# agentrpc — specification

The protocol between `panel-api` (unprivileged, user `cloudground`) and `panel-agent` (root).
It replaces the newline-delimited JSON transport of `internal/rpc`. The parameter and result
types stay in `internal/rpc` for now (see "Types" below). This document is written before the
code, and the tests in this package check it.

## 1. Transport

- HTTP/1.1 over the Unix socket at `CLOUDGROUND_AGENT_SOCKET` (default
  `/run/cloudground/agent.sock`). The path, its mode (`0660`), its group (`cloudground`) and the
  token file (`/etc/cloudground/agent.token`) do not change.
- One request per call: `POST /rpc/<Method>` with a JSON body (the parameters, or `null`).
  Connections may be kept alive and reused by the client. Calls run concurrently, one per
  connection.
- Success: `200` with the JSON result as the body.
- Failure: a non-2xx status with an `Error` body (§4). The status is derived from the error code,
  and only the body counts.

## 2. Authentication: who may connect

A connection is served only if both checks pass.

1. **Peer credentials.** On accept, before reading a byte, the server reads the peer's uid from
   the kernel (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` on macOS for development). It accepts only
   root (uid 0, used by `cgctl` and the e2e suite) and the API's uid. The API's uid is resolved
   once at start from the user `CLOUDGROUND_API_USER` (default `cloudground`), and the agent
   refuses to start if that user does not exist. Any other uid has its connection closed without
   a response, and a warning is logged with the uid. The socket's file mode stays as a first
   barrier. This check is the second one: a uid that can reach the socket file (a member of the
   `cloudground` group, for example) still gets nothing.
2. **Shared token.** Every request carries `Authorization: Bearer <token>`. The token is compared
   in constant time. A wrong or missing token gets `401 unauthorized`, and no handler runs.

Tests replace the credential lookup with a fake, because a test process runs as only one user.

## 3. Request lifecycle

For each request, in this order:

1. Method lookup in the **method table** (§5). An unknown method gets `404 not_found`.
2. The body is limited to 32 MiB, enough for a 16 MiB file upload encoded as base64. A larger body
   gets `invalid`.
3. JSON decoding into the method's parameter type. On failure: `invalid`, "bad parameters".
4. **Site validation** (when the table says `Sites`). The parameter type lists its sites
   (`Sites() []*state.Site`). Each non-nil site goes through `site.Validate(SitesRoot, s)`, which
   is the single place a site from the API is checked and confined. The first failure gets
   `invalid` with the validator's message. On success, each site in the parameters is replaced by
   its validated value (`site.Valid.Site()`), whose `RootPath` is exactly `SitesRoot/<domain>`.
   Handlers never call the validator themselves.
4a. **The agent's record** (§3.5; when `Config.Registry` is set, which the agent always does): each
   validated site goes through `Registry.Check(method, site)`; an error refuses the request before
   any lock or handler. After the handler returns, `Registry.Done(method, sites, err)` is called
   with the request's validated sites and the handler's error, still under the lock.
4b. **Parameter rules** (when the parameter type implements `Validate() error`, internal/rpc
   SPEC.md §3). An error is `invalid`, with the error's text as the message.
5. **Context.** The handler's context is the request's context, which is cancelled when the client
   disconnects, with a deadline equal to the method's `Timeout`.
6. **Site lock** (when the table says so, keyed on the first site):
   - `LockSite` takes the per-site lock (`lockSite`), for handlers that act on a site that may
     already be half gone (`DeleteSite`);
   - `LockLiveSite` takes the lock and then requires the site's root to still exist
     (`lockLiveSite`). Otherwise it fails with `conflict`, "site X no longer exists".

   A mutex wait cannot be cancelled. So once the lock is acquired, a context that has expired in
   the meantime releases the lock and fails the call, instead of running a change nobody is
   waiting for. Locks that a handler takes for a second site, or partway through, stay in the
   handler (the staging methods, for example).
7. The handler runs as `func(ctx, P) (R, error)`. A panic is recovered and reported as
   `internal`.
8. If the deadline passes or the client goes away before the handler returns, the server answers
   `timeout` (504) or `canceled` straight away and logs that the handler is still running. A
   handler that ignores its context runs to the end and keeps its lock until then. This is the
   same as before, when the client gave up after 30 minutes and the agent carried on.

### 3.5 The site index: a request's site is the one the agent provisioned

`site.Validate` checks each field on its own: it cannot tell that a request pairs site 4's id and
user with site 3's domain, and the agent would then act on site 3's tree as site 4's user (chown,
pool, slice, keys). So the agent keeps its own record, written by root only, and every site of
every request must match it (`internal/agent/siteindex.go`):

- **What it holds**: id → domain, in `/var/lib/cloudground-agent/sites.json` (root's, 0600, in the
  agent's root-only directory; `{"version":1,"sites":{"<id>":"<domain>"}}`), written atomically
  (temporary file, fsync, rename). The site user is always `cg-site-<id>` (the API names it after
  the id), so it is not stored: a site whose `system_user` is not `cg-site-<id>` is refused.
- **The check**, for each site of a request: refused (`invalid`, "the site in the request does not
  match the site this server provisioned"; the detail names both) when the index has the id with
  another domain, or the domain with another id. A known pair passes. An unknown pair (neither the
  id nor the domain recorded) is recorded when the method creates sites (`CreateSite`,
  `CreateStaging`), or when the site's root is a real directory whose group is the site user's
  (the tree the agent made for it: a site the index missed); otherwise it passes unrecorded (there
  is nothing of anyone's to protect).
- **Forgetting**: a `DeleteSite` that succeeded removes its sites; a creation that failed and left
  no tree at the site's root removes its sites (a retry gets a new id from the API).
- **Upgrade, and a lost file**: when the file is missing at the agent's start (`LoadSiteIndex`,
  before the socket opens), it is built from the trees under `SitesRoot`: each directory named
  like a domain (no dot-names, no links) whose group is a site user's `cg-site-<id>` gives
  `<id> → <domain>`; two trees of one user are both left out and logged. An unreadable file stops
  the agent with a message (move it away to rebuild it); it is never rebuilt silently over.
- **Its sibling, the database record** (Fase 9b): which database and which database account is
  which site's id, with each account's grants, is `/var/lib/cloudground-agent/databases.json`,
  managed the same way (root's, 0600, atomic, rebuilt from the server's `site_<id>` names when
  missing, never over an unreadable file). The site index makes the id of a request the site the
  agent provisioned; the database record then says what that id owns, and every handler naming a
  database or an account refuses one the record does not give it (internal/agent/ops/db SPEC §2).

### 3.7 Requests in flight

At most `Config.MaxInFlight` (default 64) authenticated requests are served at once; the slot is
taken after the token check, before the body is read, and released when the answer is written.
One more gets `503 unavailable` at once, "The agent is busy; try again in a moment" (logged with
the limit), without running anything. No per-method weights: the API is the only caller besides
root, and a burst past 64 is a bug or an attack, not load (R-10.5).

## 4. Errors

```json
{"code": "invalid", "message": "invalid snapshot id", "detail": "…"}
```

| code | meaning | HTTP (agent socket and API) |
| --- | --- | --- |
| `invalid` | the request is wrong; `message` says how | 400 |
| `not_found` | the named thing does not exist (also: unknown method) | 404 |
| `conflict` | the state forbids it now (site gone, already exists, …) | 409 |
| `unauthorized` | bad token | 401 |
| `timeout` | the method's deadline passed | 504 |
| `canceled` | the caller went away | 499 on the socket; the API never answers with it |
| `unavailable` | the agent could not be reached (client side) | 503 |
| `internal` | everything else | 502 from the API (500 on the socket) |

- `message` is **safe to show to a user**. It contains no command line, no absolute path of the
  host, no stderr from a tool, and no secret.
- `detail` is for logs only. It goes to the agent's journal and to the API's journal (the client
  logs it once per failed call). It never goes to a browser or into a task's stored error.
- A handler marks a message as user-safe by returning an `*agentrpc.Error`, built with
  `Invalid`, `NotFound`, `Conflict` or `Wrap`, anywhere in its error chain. The server takes code
  and message from the first such error and puts the whole chain's text in `detail`.
- Any other error is `internal`. Its message is the method's `Fail` text from the table (for
  example "Could not list the files") and its `detail` is the original error text.
- `key` and `args` (optional) name the message for a translation: `key` is a key of the API's
  catalogue (`internal/i18n`) and `args` its values, in order, as text. `message` is still the
  English text, for logs and for any caller that does not translate. The same rules apply to
  `args` as to `message`: safe to show.
- `(*Error).Error()` returns `message` only. So an API task that wraps an agent error
  (`fmt.Errorf("remove %s: %w", …)`) stores and shows only safe text.
- The API turns agent errors into HTTP responses through one helper (`respondAgentErr`), which
  maps the code to a status as in the table above (an optional default replaces 502 for
  `internal`) and sends `message` only.

## 5. The method table

One table, in `methods.go`, is the single source for both the client and the server. Each entry
declares:

| field | meaning |
| --- | --- |
| `Name` | the wire name (unchanged from `internal/rpc`'s method constants) |
| `Timeout` | the server-side deadline. The client waits `Timeout` plus a 30 s grace, so the server's typed `timeout` arrives first |
| `Sites` | the parameters carry sites, which are validated centrally (§3.4). The server refuses at registration to have a parameter type that implements `Sites()` declared without it, and the reverse |
| `Lock` | `LockNone`, `LockSite` or `LockLiveSite` (§3.6) |
| `ReadOnly` | the call changes nothing. Only for these does the caller's cancellation reach the agent. A mutating call, once sent, is detached from the caller's cancellation (but keeps the timeout), so a browser that closes the tab never leaves the API unaware of a change the agent made |
| `Fail` | the user-safe message for an `internal` error |

The server panics at start if a handler is registered for a method that is not in the table, or
registered twice. A test checks that the agent registers every method in the table.

## 5a. Stopping

`Server.Shutdown(ctx)` stops accepting connections and waits, until `ctx`
ends, for every request in flight to be answered (`http.Server.Shutdown`).
`panel-agent` calls it on SIGTERM with a 5 s ceiling, so a restart never cuts
a reply off. A self-update's reply, for one, would otherwise race the restart
its guard issues.

## 6. Client

- `agentrpc.Call[P, R](ctx, caller, method, params) (R, error)` returns the typed result. The
  `Method[P, R]` value carries the types, so a mismatch is a compile error.
- `agentrpc.Into(ctx, caller, method, params, &out)` is the same call, decoding into an existing
  variable (or nothing, with `nil`). The API uses it so that its call sites keep their shape.
- `Caller` is the untyped seam, `Call(ctx, name, params, out) error`, implemented by `*Client` and
  by the API's test fake.
- Dialling retries 5 times, 100 ms apart, to cover the moment between systemd marking the agent
  active and the socket being bound. A failure to reach the agent is `unavailable`.
- Every error the client returns is an `*Error`.

## 7. Types

The parameter and result structs stay in `internal/rpc` for this step. Moving them would touch
every handler, every API call site and every test, and the domain rewrite in Phase 3 replaces them
per domain anyway. The method name constants and the old transport (`client.go`, `server.go`,
the handshake) are deleted: the method table here is the only list of methods.

## 8. Not in this step

- Handlers receive the validated site inside their parameters, not as a separate `site.Valid`
  argument. Typed per-domain handler signatures come with Phase 3.
- Context plumbing beyond the handler entry. Handlers that used to start from
  `context.Background()` now use the request's context. Their callees are unchanged.
