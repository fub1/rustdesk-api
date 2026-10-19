# Incremental Go architecture

This repository starts from alonginwind/rustdesk-api revision
`4e5883219bbc4c4683dbc0dbb4b9a86f1aae48c0`, retaining its history, Gin, GORM
and the existing client wire contracts. It remains a single API process.

## ARCH 01: resource construction, handler construction and running

The command bootstrap initializes legacy resources and checks database
migrations. Only the server command creates its login limiter. `http.NewHandler`
borrows the initialized resources and returns a real `*gin.Engine` without
listening, migrating, connecting to databases/Redis, doing OIDC discovery or
starting workers. A maintenance command does not start the login limiter or
heartbeat flusher. The flusher starts only after handler construction succeeds.

`http.Run` performs the listening step and returns its error to Cobra, which
causes a nonzero process exit. Unix retains the upstream endless listener;
Windows retains Gin's listener. Full bounded shutdown, heartbeat flushing and
resource ownership are the later ARCH 06 work, not claimed by this slice.

Gin process mode is configured at bootstrap. Handler construction leaves mode
and default writers unchanged. Panic recovery logs only static metadata through
the captured logger, without a request dump, panic value or pipe worker.
Templates are validated up front and
resolved under the configured `gin.resources-path/templates`; this corrects the
previous hardcoded working-directory lookup. Debug template reload is retained.
Missing resources, inconsistent service installation, missing/broken templates
and invalid trusted proxies return errors before listening.

This handler retains a **legacy adapter** for existing business controllers.
ARCH 02 additionally requires an explicitly constructed session HTTP adapter.
Legacy controllers still read `global` and `service` package state. The
constructor checks that these represent the same installation; the entire
business router does not provide instance isolation. Nil
embedded service receivers remain valid under the upstream design and are not
mistaken for missing dependencies. Bootstrap installs resources once, before
requests; tests save/restore that state and are not parallelized in this package.

### Route ownership

| Registration owner | Owned paths | Current implementation |
| --- | --- | --- |
| `router.WebInit` | `/`, `/_admin/*`, optional webclient assets/config | Legacy |
| `router.Init` | `/api/admin/*`, optional admin Swagger | Legacy registration, injected session/role checks |
| `router.ApiInit` | RustDesk `/api/*`, `/upload/*`, optional client Swagger | Legacy registration, injected session checks |
| `internal/httpapi` | Client/admin current-user, logout, admin token delete/batchDelete | Instance-owned sessions |

Registration stays `WebInit → Init → ApiInit`, including the existing group
middleware order, with admin logout now explicitly registered after session
authentication. ARCH 02 transfers current-user, logout and direct session
revocation to the injected handler and removes their old registrations. A route
must have exactly one owner. Anonymous device reporting stays anonymous.

Checked-in route inventories preserve all 166 default method/path pairs and
175 pairs with WebClient/Swagger enabled. Real HTTP tests preserve the public
API response, plain-text 404, client missing-token HTTP 401 JSON response and
admin missing-token HTTP 200 with body `code=403`. They do not certify the
all existing resource-level business rules. ARCH 02 adds real authenticated
HTTP tests for both header formats, revocation and current role checks.

## ARCH 02: instance-owned session authentication

Composition is `cmd → internal/app.NewSessions → authstore/sessiontoken →
auth.Service → httpapi.Handler → router`. The auth core imports neither Gin,
GORM, application globals nor database models. Store, local-token verifier,
clock and TTL are explicit dependencies. Each store owns its DB reference;
each verifier copies its local JWT key. The HTTP adapter snapshots translator
and route-hint dependencies and uses explicit response DTOs. A narrow bridge
installs the authenticated user in legacy request context for unmigrated routes.

Client Bearer and admin api-token now use one DB-authoritative decision:
unambiguous session, unexpired lifetime, existing enabled user, and current
database role. Configured local JWTs additionally require HS256, expiration
and matching user_id. A JWT cannot bypass DB revocation or extend its signed
lifetime through DB renewal. Administrator permission is independent of which
header carried the credential. The formerly JWT-only middleware delegates to
this same path. Management commands now require administrator permission;
ordinary users retain their own management routes.

Two simultaneously live session HTTP adapters with independent SQLite stores
are tested without changing globals between requests. These tests cover both
opaque and same-key JWT sessions, cross-store rejection, current-user responses,
role changes, logout and direct token revocation. They do not claim that legacy
password/LDAP/OIDC issuance, config presentation or all business CRUD support
independent full application instances. Details, rollout semantics and remaining
work are in [auth-sessions.md](auth-sessions.md).

## ARCH 03: bounded requests and safe error reporting

The session HTTP adapter applies `app.auth-timeout` (default 5 seconds, negative
values rejected) to each authentication/revocation operation, derived from the
original request context. Earlier parent deadlines and cancellation are retained.
GORM operations and transactions use that context. Cancellation/deadline takes
precedence if a dependency returns another error after the context has expired.
There is no detached database goroutine returning a fake timeout while continuing
to grant access or mutate sessions in the background.

Authentication, disabled user, forbidden, input error, dependency outage,
cancellation and deadline have separate stable classifications. New session
responses never include SQL, token, DSN or error-cause text. The optional failure
observer receives only operation, classification and matched route template.

Request/recovery logging omits query strings, concrete path parameters, headers,
cookies, body and panic values. Operation logging omits sensitive resource bodies,
retains bounded target metadata and redacts ordinary structured details before
truncation. This protects these common logging outlets; legacy OIDC/provider
errors and other business logging still require review as those flows migrate.

Management Socket commands now inherit the request context and share one 5-second
IPv6/IPv4 budget. Connected reads/writes have deadlines, cancellation closes the
connection, and fallback is allowed only before a connection succeeds. A write
followed by failure is not replayed. The existing command/response protocol and
legacy command business service remain in place; complete stream framing and
server lifecycle are later work.

Real SQLite plus GORM operation callbacks exercise in-flight query, renew and
revoke cancellation/deadlines, dependency errors and rollback. These tests verify
context propagation and persisted outcomes, not the interrupt latency of every
database driver's native slow query. HTTP tests verify 408/504/503 responses and
safe failure events; temporary loopback servers verify Socket budgets/no replay.

## ARCH 08: isolated verification

Push and pull-request validation runs all packages with the race detector,
Redis integration, vet and builds. It uses a fixed Go toolchain and committed
checksums, without the previous floating self-module replacement. The original
module path is retained so this change does not churn every Go import.
Actions dependencies are pinned to commit SHAs. A validation artifact is built
from the tested revision and records its SHA and toolchain. Details and local
commands are in [testing.md](testing.md); validation does not publish releases.

## Next slices

ARCH 04/05 cover
identity/session transactions and atomic OIDC login attempts; the startup
migration failure portion is already documented in
[migration-recovery.md](migration-recovery.md). ARCH 06 completes lifecycle
management, and ARCH 07 handles the first client compatibility fixes.

Our API will act as a standards-compliant OIDC relying party adapting to our
OIDC provider's requirements (including public client, no secret and S256).
The provider must not be changed to accommodate assumptions in this fork.
No OIDC behavior is changed by the current foundation work.
