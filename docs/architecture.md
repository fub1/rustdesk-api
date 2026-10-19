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
heartbeat flusher. The flusher starts only after handler construction and
listener binding succeed.

`http.RunContext` performs the listening and owned-worker steps, returning errors
to Cobra. ARCH 06 now uses a standard HTTP server on both Unix and Windows,
with explicit request drain, worker stop/join and resource release. Its forced
timeout behavior and platform limitations are in [lifecycle.md](lifecycle.md).

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
| `internal/httpapi` | Client/admin current-user, logout, token delete/batchDelete, OIDC begin/callback/poll/bind/info/confirm/unbind/myOauth | Instance-owned sessions and login attempts |

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

## ARCH 04: committed issuance and explicit identity links

An instance-owned `auth.Issuer`, cryptographic token issuer and GORM issuance
store now commit the local session, login log (referencing the actual token
row ID) and existing peer binding in one transaction. The current user must
still exist and be enabled. Password logins also compare the password hash
that was authenticated, rejecting a concurrent password change. Opaque sessions
use 32 random bytes; signed sessions use the same random material for their jti.
No token is returned after a persistence or commit failure.

The legacy password/LDAP HTTP flows call this issuer through an explicitly
installed bridge with a request-derived budget. Password change plus session
revocation, and user deletion plus identity/session/device cleanup, are atomic.
Legacy account lookup/LDAP verification and ordinary business CRUD remain legacy.
The old email-based OAuth registration helper is disabled.

Database version 275 adds only `oidc_identities`: id, issuer, sub, user_id,
with a unique issuer/sub key and a user foreign key. This model was explicitly
approved before implementation. Existing UserThird/op/email data is not copied.
SQLite connections now enable foreign keys on every pooled connection. MySQL
uses byte-exact binary issuer/subject columns to preserve case and trailing
spaces; real MySQL/PostgreSQL migration verification remains outstanding.

The identity adapter provides admission/binding transactions for ARCH 05.
Registration-closed identities are rejected; explicit auto_register creates a
new ordinary account, never an account selected by email or username. Existing
users bind while authenticated; binding rechecks the initiating session and
enabled user within the transaction. A unique identity cannot bind to two users.
User, identity, token, login log and peer changes share the login transaction.
Provider identity/client changes invalidate a pending snapshot. The network
protocol and browser challenge routing are the next slice.

## ARCH 05: atomic browser attempts and public OIDC

Composition adds `app.NewOIDC → identitystore / oidcprovider / loginattempt →
oidclogin.Service → httpapi`. Discovery is lazy and does not run at bootstrap.
The core coordinates explicit stores/protocols and does not import Gin, GORM or
legacy globals. Callback claim, authorization and issuance are distinct atomic
states, with independent polling/state/nonce/verifier credentials, bounded
capacity and an absolute TTL. A lost poll response retrieves the same committed
session; every retrieval still checks the authoritative local session/user.

All old federated callback/login aliases, manual confirmation, binding, cache
info and myOauth registrations now delegate to these instance-owned handlers.
There is no fallback into the old cache controllers. Password/LDAP credential
verification and business CRUD remain legacy; their new issuance bridge is
described above. The webSSO flow is retained with a distinct flow purpose and
live local approval, including source-session verification in the commit.

The network adapter enforces the existing IdP's PUBLIC/none, Code/S256,
openid/profile contract with mandatory verified ID Tokens and matching UserInfo
subjects. It uses bounded request-context network calls, no redirects and no
refresh workflow. Local admission links issuer/sub without email matching or
external administrator claims. Attempt/source-session deadlines also bound
binding and issuance transactions. Details, configuration, rollout restrictions
and verified test scope are in [oidc.md](oidc.md).

The first state backend is bounded process memory, explicitly single-instance;
production IdP registration/deployment and real Flutter/browser acceptance are
still required. Real TLS protocol + real SQLite + production router tests do
not establish those external deployment facts or multi-instance support.

## ARCH 06: owned serving and shutdown

`internal/lifecycle` binds before starting workers, reports startup failures,
drains in-flight requests and stops workers within a shared shutdown budget.
`internal/heartbeat` owns latest-observation buffering and retries failed atomic
batches through a context-aware GORM adapter. The command closes resources only
after successful drain; incomplete forced shutdown exits without racing cleanup
against residual work. The operation-log write is bounded and tracked by its
HTTP request rather than a detached goroutine. Maintenance commands create no
server/cache workers. See [lifecycle.md](lifecycle.md) for verified behavior and
the distinction between graceful completion and a forced process exit.

## ARCH 07: selected client compatibility boundaries

`internal/clientcompat` owns scoped pagination and typed address-book patches;
`internal/clientaudit` owns atomic audit/receipt writes and exact uint64 session
IDs; `internal/clientstrategy` owns effective policy snapshots and durable
managed-key history. Each receives a database/context explicitly and starts no
workers. Controllers and narrow legacy services remain the assembly bridge;
this does not remove globals from all business CRUD or make the whole server
safe for independent instances in one process.

The three approved model changes ship as versions 276–278. Selected HTTP,
transaction, commit failure, retry and upgrade tests use real SQLite. The client
matrix records partial coverage and missing interfaces, rather than equating
registered routes with working features. See [address-book compatibility](addressbook-compatibility.md),
[audit semantics](client-audit.md), and [the client matrix](client-compatibility.md).

Policy deletion uses explicit empty strings only for historical managed keys,
while retaining values from still-effective policy and leaving unmanaged keys
alone. Restoring pre-policy personal values or exact client-customized defaults
has been removed from required future work; the clearing behavior and its
limits are the accepted final semantics. Existing protocol audit events lack
connection generations, so unseen delayed events retain documented ambiguity.

## Remaining acceptance

The startup migration failure portion is documented in
[migration-recovery.md](migration-recovery.md). Real Rust/Flutter client and
MySQL/PostgreSQL validation remain separate gates. The seven missing fixed
client endpoints continue in their planned later phases.

Our API will act as a standards-compliant OIDC relying party adapting to our
OIDC provider's requirements (including public client, no secret and S256).
The provider must not be changed to accommodate assumptions in this fork.
ARCH 04/05 now establish transactional login and the standard OIDC implementation;
first-phase production acceptance remains subject to the deployment and client
checks above.
