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
and default writers unchanged. Recovery logging uses a synchronous adapter
instead of opening a logrus pipe worker. Templates are validated up front and
resolved under the configured `gin.resources-path/templates`; this corrects the
previous hardcoded working-directory lookup. Debug template reload is retained.
Missing resources, inconsistent service installation, missing/broken templates
and invalid trusted proxies return errors before listening.

This handler is explicitly a **legacy adapter**. Existing controllers still
read `global` and `service` package state. The constructor checks that these
represent the same installation; it does not provide instance isolation. Nil
embedded service receivers remain valid under the upstream design and are not
mistaken for missing dependencies. Bootstrap installs resources once, before
requests; tests save/restore that state and are not parallelized in this package.

### Route ownership

| Registration owner | Owned paths | Current implementation |
| --- | --- | --- |
| `router.WebInit` | `/`, `/_admin/*`, optional webclient assets/config | Legacy |
| `router.Init` | `/api/admin/*`, optional admin Swagger | Legacy |
| `router.ApiInit` | RustDesk `/api/*`, `/upload/*`, optional client Swagger | Legacy |

Registration stays `WebInit → Init → ApiInit`, including the existing group
middleware order. There are **no new auth-owned routes in this slice**. When
ARCH 02 transfers a method/path to a new auth handler, remove that registration
from the legacy owner in the same change. A route must have exactly one owner.

Checked-in route inventories preserve all 166 default method/path pairs and
175 pairs with WebClient/Swagger enabled. Real HTTP tests preserve the public
API response, plain-text 404, client missing-token HTTP 401 JSON response and
admin missing-token HTTP 200 with body `code=403`. They do not certify the
existing authenticated business rules; auth/session/security changes follow.

## ARCH 08: isolated verification

Push and pull-request validation runs all packages with the race detector,
Redis integration, vet and builds. It uses a fixed Go toolchain and committed
checksums, without the previous floating self-module replacement. The original
module path is retained so this change does not churn every Go import.
Actions dependencies are pinned to commit SHAs. A validation artifact is built
from the tested revision and records its SHA and toolchain. Details and local
commands are in [testing.md](testing.md); validation does not publish releases.

## Next slices

ARCH 02 introduces explicit instance dependencies for new authentication,
shared local session validation and framework-independent auth use cases.
ARCH 03 adds error/context/timeout boundaries to those paths. ARCH 04/05 cover
identity/session transactions and atomic OIDC login attempts; the startup
migration failure portion is already documented in
[migration-recovery.md](migration-recovery.md). ARCH 06 completes lifecycle
management, and ARCH 07 handles the first client compatibility fixes.

Our API will act as a standards-compliant OIDC relying party adapting to our
OIDC provider's requirements (including public client, no secret and S256).
The provider must not be changed to accommodate assumptions in this fork.
No OIDC behavior is changed by the current foundation work.
