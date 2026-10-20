# Local session boundary (ARCH 02)

Current policy: [SEC07 / SEC08 / PRE01 decisions](session-and-preset-decisions.md).
The signed-session absolute lifetime is confirmed; opaque and disable/re-enable
policy remain separate decisions. Local email/TOTP verification is outside the
development scope by the latest user decision.

The original ARCH 02 slice unified authentication of **already issued local sessions**, current
user responses, logout and direct administrator session revocation. It reuses
the existing users/user_tokens/peers schema. It does not introduce identity,
purpose or token-hash fields. ARCH 04/05 now add committed issuance and a separate
issuer/sub relation; the current login behavior and migration rules are described
in [oidc.md](oidc.md).

## Rules and compatibility

Client `Authorization: Bearer ...` and admin `api-token` resolve through the same
instance-owned service and store. Client schemes are validated, and ambiguous
headers or duplicate raw-token rows are rejected. The store must contain exactly
one matching session with a nonzero user, `expired_at > now`, and an enabled user.
Roles come from the current database user on each request. No email is required.

Without a local JWT key, existing opaque DB sessions remain usable. With a key,
both headers require valid HS256, a required signed expiration, and `user_id`
equal to the DB session's user. The local session JWT is not an OIDC ID/access
token. Provider tokens are not accepted merely because they are signed.

New signed sessions use `app.token-expire` for both JWT exp and DB expiry:
this is their fixed absolute lifetime, defaulting to 168 hours for zero. Activity
and autoLogin do not extend that signature, and no refresh token is issued.
The legacy `jwt.expire-duration` does not control current instance issuance.
Negative TTL prevents construction. Without a signing key, opaque sessions still
use a sliding DB lifetime: below one third of the TTL they may renew an existing
live row. That branch currently has no absolute cap and needs a separate policy
decision. Renewal never recreates a revoked row or shortens a concurrent extension.
All internal instants are UTC with the legacy Unix-second expiry precision.

New legacy JWT issuance includes a random jti so same-user, same-second logins
do not produce identical credentials. Previously duplicated token rows are
rejected; their users must log in again. Enabling/changing a JWT key or deploying
the newly consistent signed-expiry checks can invalidate previously accepted
admin sessions; plan a fresh login during rollout. Existing valid, unambiguous
sessions need no table migration. ARCH 04 new opaque sessions now use 32 random
bytes. Token hashing, opaque absolute lifetimes and stored session-purpose
isolation remain separate policy/schema work; OIDC attempts already enforce their
own absolute TTL and flow/audience isolation.

Client authentication failure retains HTTP 401 with an error object. Admin
missing/invalid sessions retain HTTP 200 with body code 403; disabled users
retain HTTP 401. Database failures return HTTP 503 with fixed public text rather
than a false login failure. Request cancellation and deadline errors remain
distinct and never turn into anonymous optional-auth success. The public admin
config endpoint returns only title for no/invalid identity; it may show hello
only after successful instance session validation.

## Ownership and revocation

The injected HTTP handler owns these method/path implementations:

- GET `/api/user/info`, POST `/api/currentUser`, POST `/api/logout`.
- GET `/api/admin/user/current`, POST `/api/admin/logout`.
- POST `/api/admin/user_token/delete` and `/batchDelete`.

All protected client/admin groups use the same service. Admin role gates use
the authenticated database principal, including all four RustDesk command
routes and existing OAuth configuration CRUD. Ordinary users may still access
current-user and their existing personal management routes.

Logout atomically removes the exact stored session and preserves the fork's
existing side effect: unbind the first peer matching its stored device UUID and
user. It returns success only after commit. Revising device ownership semantics
is later business work. Direct administrator ID deletion remains separate and
does not unbind devices; it checks current administrator status and uses the
instance store. Deleting an already absent ID now returns idempotent success.
Empty/malformed HTTP deletion requests are rejected.

## Request bounds and error reporting (ARCH 03)

`app.auth-timeout` configures the budget for each session authentication,
logout or administrator revocation operation. The default/zero value is 5 seconds;
negative values fail construction. Each operation derives a child context from
the request, without changing the whole legacy request's lifetime. A parent
deadline can shorten this budget. SQL queries, renewal and revocation transactions
all receive it. Cancellation maps to HTTP 408; a deadline maps to HTTP 504;
dependency failures map to 503. They are never converted into a credential error
or optional anonymous success, and no underlying cause is returned to the client.

The injected failure observer receives only a stable operation, result code and
matched route template. The standard bootstrap logs those fields at DEBUG level.
It receives no raw error/token/user snapshot. Request logs use matched templates;
unmatched routes use a constant placeholder. Recovery never formats a panic value
or dumps headers/query/body/cookies. Sensitive operation-log resources retain
only bounded target metadata, while ordinary structured details are recursively
redacted before truncation; malformed/oversized data is omitted. Existing stored
operation-log rows are not retroactively sanitized by this source change.

Connected hbbs/hbbr management calls share a 5-second total budget and inherit
request cancellation. Only a failed dial can fall back to another address;
once connected, failed writes/reads do not trigger command replay. A timeout
after sending means the outcome may be unknown, and the response reports failure
without claiming the remote command was rolled back. Full legacy request/OIDC
timeouts, login-flow TTLs and graceful shutdown remain their separate later work.

## Login changes and remaining boundaries (ARCH 04/05)

Password/LDAP credential verification and local registration remain legacy
composition, but their final session issuance now uses the instance issuer:
session, actual login-log reference and existing device binding commit together.
Current enabled status and authenticated password hash are rechecked. Password
change and session revocation, and user deletion/identity/session/peer cleanup,
are transactional. Other device/business mutation paths and session listing
remain legacy. Local registration may create an account before its subsequent
automatic login fails; the response never claims an uncommitted session exists.
Full application instance isolation is not claimed.

The standard OIDC RP now adapts to our provider: public client, no client secret,
Code/S256, required protocol validation and issuer+sub identity mapping. It
reuses local session/user admission rules. Email is optional, and neither UI
route hints nor provider profile claims grant administrator rights. Our IdP is
not changed to satisfy assumptions in the fork. Database version 275 adds the
explicitly approved identity relation. Old op/email links are not migrated;
OIDC users require explicit binding or configured ordinary-account registration.
The old OIDC/cache/confirmation controllers are no longer registered, including
their aliases. New native/admin polling retains the expected response shapes,
checks device and route audience, and revalidates every cached local session.
Ordinary OIDC login has since passed on the actual Ubuntu 1.5.0 client; that does
not accept logout/disable/password-change interactions or the later patches.
Current IdP logout limits and the remaining live acceptance are recorded in the
[decision record](session-and-preset-decisions.md); see [oidc.md](oidc.md) for protocol details.
