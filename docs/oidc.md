# Standard OIDC login (ARCH 05)

The API is a public OIDC relying party for the existing identity service. It
uses authorization code + S256, `token_endpoint_auth_method=none` and only
`openid profile`. No client secret, placeholder secret, Basic authentication,
offline access or refresh-token workflow is used. External provider roles never
grant RustDesk administrator rights. Email is optional and never links accounts.

## Configuration and registration

Register a separate RustDesk client through the IdP's normal controlled app
management process. Register the exact HTTPS callback:

```
https://YOUR-API-BASE/api/oidc/callback
```

`rustdesk.api-server` supplies the API base, including any externally routed
prefix; the application appends `/api/oidc/callback`. Select one issuer exactly,
including its path/trailing slash, and use its Discovery endpoints consistently.
Discovery, authorization, token, UserInfo and JWKS endpoints must use HTTPS.
Use a publicly trusted certificate (or an explicitly configured system trust
store). There is no TLS-verification bypass.

Create/update the existing admin OAuth resource using its authenticated admin
API; this example is the JSON body, with no credentials embedded:

```json
{
  "op": "our-oidc",
  "oauth_type": "oidc",
  "issuer": "https://YOUR-IDP-ISSUER",
  "client_id": "YOUR-REGISTERED-CLIENT",
  "client_secret": "",
  "scopes": "openid,profile",
  "pkce_enable": true,
  "pkce_method": "S256",
  "auto_register": false
}
```

For updates include the resource `id`. OIDC configuration defaults to S256,
openid/profile and registration closed; secret/email/offline scopes, plain PKCE
and explicitly disabled PKCE are rejected. Updating to an empty secret actually
clears the old database value. Existing invalid configurations require an
explicit administrator update; they are not silently downgraded or expanded.
The previously bundled web admin may still demand a secret or default PKCE to
false: update that independent UI or use the admin API for configuration.

PUBLIC clients do not require the IdP's App CIDR/Entitlement checks. Normal
network access, reverse proxy configuration and precise redirect registration
still apply. Do not rerun empty-database bootstrap or reuse another app's client.

## Local identity and admission

Database version 275 adds the user-approved `oidc_identities` relation, one row
per `(issuer, sub)` identity and its local `user_id`. The pair is unique and the
user reference is a foreign key. No profile/email/admin snapshots or timestamps
are added to the relation. Old `user_thirds` records remain historical data and
are never automatically interpreted as verified issuer/sub identities.

With registration closed, an unlinked identity cannot log in. Sign in to an
existing local account, then POST `/api/admin/oauth/bind` with its `op` using
that live api-token and complete the returned authorization URL. The callback
revalidates that exact initiating browser session and atomically attaches the
verified identity. It does not create another session. Another user's existing
identity cannot be taken over. Old code-only `bindConfirm` is rejected.

With explicit `auto_register=true`, an unlinked verified identity creates a new
ordinary enabled user in the existing default group. It does not select a user
by email/name and does not initialize a shared local password. A random suffix
keeps the new username independent from any existing login name; subsequent
logins use issuer/sub regardless of profile changes. Admission, identity link,
local session, login log and existing device link commit together. The next
request uses the current local user status and role.

The normal myOauth response now reads these explicit links. Unbinding requires
the current authenticated session; an OIDC-only user cannot remove their final
identity without another local or currently configured linked login method. Existing administrator
password-reset recovery remains available. IdP logout/disable does not instantly
revoke an already issued local RustDesk session; local revocation/expiry and a
future separately verified notification policy govern that boundary.

## Browser and polling behavior

Native `/api/oidc/auth` and admin `/api/admin/oidc/auth` retain their JSON wire
formats. The returned polling `code` is independent from OAuth `state`; each
attempt also has a separate nonce and PKCE verifier, all generated with 32
cryptographic random bytes. The server captures id/uuid, platform and the route's
client/admin audience. Caller-controlled deviceInfo.type never grants a role.

Callback state is claimed exactly once before exchanging the authorization code.
Missing/wrong nonce, ID Token/signature/issuer/audience/expiry/issued-at/azp,
UserInfo subject mismatch or provider refusal cannot authorize a local session.
If an authorization-response `iss` is present it must match the captured issuer.
The ID Token is mandatory. Changing a provider identity/client/admission policy
invalidates an unissued attempt's configuration snapshot. No token response or
provider error body is rendered or logged by these adapters.

Polling validates code + original id/uuid + server route audience + flow purpose.
Pending retains `error: "No authed oidc is found"`. Successful issuance happens
at most once per attempt; a lost response can be retried with the same polling
credentials to retrieve the same committed token within the attempt lifetime.
Every retrieval authenticates that token again, so logout, local disable, user
deletion and administrator revocation cannot be bypassed by the result cache.
Database failure leaves an authorized attempt retryable; failed exchange is
terminal and never retries the single-use authorization code.

The original webSSO option uses a separate `web-login` purpose and explicit
approval from a live, non-default-password account. It cannot approve an OIDC
attempt. Its source session is checked again inside the issuance transaction.
Empty/default-password accounts cannot approve another device. No challenge
response exposes nonce, verifier, source session token or raw provider profile.

## Runtime and verification boundary

The attempt store is **single-process memory**, capped at 1,024 attempts and an
absolute five-minute TTL, with lazy cleanup and no per-attempt workers. A server
restart loses pending attempts and retained login results; restart authorization
in that case. Deploy one API instance for this feature. Independent replicas
do not share state; sticky routing alone is not a delivered multi-instance or
failover solution. A shared atomic state backend is separate work.

Operations inherit the request context and `app.auth-timeout` (default five
seconds); attempt expiry and source-session expiry further bound sensitive
operations. Discovery/exchange/JWKS/UserInfo requests have an actual HTTP timeout,
reject redirects and preserve cancellation. JWKS is fetched for each exchange,
so no background fetch continues after request cancellation and key rotation
does not depend on a stale local key cache. This trades extra login-time network
requests for simple bounded behavior; ordinary API authentication stays local.

Tests cover real TLS Discovery/token/JWKS/UserInfo, real SQLite transactions,
concurrent callbacks/polls, response recovery, faults/cancellation, and the
production router's native/admin/local-session contracts. They do not establish
production IdP deployment/registration, browser UI usability, a real Flutter
binary, IdP single logout, or MySQL/PostgreSQL runtime coverage. The known IdP
compatibility fix baseline is dev `cf55cc21f9c604818cffda8faeab1230947ac67a`
([PR #67](https://github.com/fub1/ooooidc/pull/67)); a dev merge is not deployment.

GitHub/linux.do/other legacy OAuth provider implementations remain in source,
but their old login/cache/binding controllers are no longer registered. Only
the standard public OIDC configuration is advertised for federated login in
this slice. Porting other providers requires their own explicit adapters and
tests, without weakening this OIDC validation path. Route aliases remain for
compatibility and delegate to the same new implementation.
