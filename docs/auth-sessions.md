# Local session boundary (ARCH 02)

This slice unifies authentication of **already issued local sessions**, current
user responses, logout and direct administrator session revocation. It reuses
the existing users/user_tokens/peers schema. It does not introduce identity,
purpose or token-hash fields, and does not certify the legacy login flows.

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

The configured `app.token-expire` remains the DB sliding lifetime. Zero now
correctly defaults to 168 hours (the previous issuance fallback was nanoseconds);
negative TTL prevents session construction. Remaining lifetime below one third
of the configured TTL may renew an existing live DB row. Renewal never creates
a missing/revoked row or shortens another request's extension. Signed sessions
are capped by JWT expiration, so a DB renewal cannot revive an expired JWT.
All internal instants are UTC with the legacy Unix-second expiry precision.

New legacy JWT issuance includes a random jti so same-user, same-second logins
do not produce identical credentials. Previously duplicated token rows are
rejected; their users must log in again. Enabling/changing a JWT key or deploying
the newly consistent signed-expiry checks can invalidate previously accepted
admin sessions; plan a fresh login during rollout. Existing valid, unambiguous
sessions need no table migration. A new opaque issuer, token hashing, absolute
lifetimes and purpose isolation remain separate session-policy/schema work.

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

## Boundaries for the next slices

Password/LDAP login, registration, legacy OIDC challenge/callback/poll and their
final session issuance still use legacy composition. Their persistence errors,
identity binding, concurrency and credential generation need ARCH 04/05; the
current change must not be described as a secure or instance-isolated full
login flow. Password changes/device removal still revoke via legacy business
transactions against the same deployment DB. Session listing and other business
controllers remain legacy too. No full application multi-instance claim is made.

The standard OIDC RP will adapt to our provider: public client, no client secret,
Code/S256, required protocol validation and issuer+sub identity mapping. It will
reuse local session/user admission rules. Email is optional, and neither UI
route hints nor provider profile claims grant administrator rights. Our IdP is
not changed to satisfy assumptions in the fork. No new OIDC network adapter or
identity-link schema is introduced until its flow and business rules are implemented.
