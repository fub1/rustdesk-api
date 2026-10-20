# User management authorization and session revocation

The existing account create, update, delete, administrator password reset and
current-user password change routes use an instance-owned `useradmin.Store`.
The URLs, management JSON fields and response envelope remain available. This
slice adds no database model, column or migration; runtime version remains281.

Before a business write, the transaction checks the actor's current enabled
state, administrator role when required, unique raw token, session identity,
stored expiry and signed expiry. Middleware authorization alone is insufficient.
Current-user password changes use the locked current password. A wrong old
password cannot overwrite it; accounts with an empty stored password retain
their existing set-password behavior. Metadata forms cannot write passwords,
timestamps or other ORM fields.

At least one enabled administrator must remain after deletion, disablement or
demotion. Disabled administrators do not count. All relevant existing user rows
lock in ascending primary-key order; administrator-set drift fails safely for
an explicit retry rather than taking a late user lock. The legacy service update
and delete wrappers use the same guard, including the path used by LDAP sync.
An operation may affect the actor's own account when another enabled
administrator remains. Authentication is checked before mutation, so successful
self-deletion, self-disablement, self-demotion or password change is not rolled
back merely because the operation intentionally invalidates its own session.

Disabling a user atomically revokes all that user's local sessions, including
sessions issued after OIDC login. Re-enabling also revokes historical leftover
tokens for an already disabled/non-enabled account, so upgrading cannot restore
its old credentials. Enabled-to-enabled metadata edits do not revoke sessions.
Password reset and current-user password change retain their existing all-local-
sessions revocation. Logout still revokes its current session. These operations
do not invoke IdP logout or terminate established remote-control connections.

Legacy password-hash upgrades compare and replace only the hash actually
verified. A concurrent password reset or failed SQL update cannot return a stale
user credential for session issuance. Issuance still checks the current stored
hash before committing a new session. Upgrade SQL uses the silent credential
logger; a successful retry must verify the current password again.

Target-session revocation shares the token-index NOWAIT protocol with checked
user cleanup. Busy token groups return a safe unavailable error with no partial
business changes; retry after the conflicting operation finishes. All writes,
including password changes and session deletion, commit together. Collection
reference rejection and checked child cleanup remain in the shared lifecycle
implementation. SQL, cancellation and actual COMMIT failures roll back.

Account metadata updates preserve the legacy nonzero-field patch behavior:
omitted/zero status or group, empty strings and nil administrator flags preserve
the stored value. An explicit false administrator flag demotes. Creation rejects
request-supplied IDs and names that normalize to empty, and keeps the existing
lowercase/ASCII-space-removal formatting and initial password behavior.

For creation, LDAP namespace checks use an injected configuration snapshot,
check the raw and actual normalized name, escape the LDAP filter, and obey the
request budget through dial, TLS, bind and search. Directory failure fails closed
without a local write; a disabled directory leaves local creation available.
The broader legacy LDAP authentication and sync implementation is not replaced.
Updating an existing username into a reserved LDAP namespace remains separate
work; this slice does not claim all username changes enforce that reservation.

Automated HTTP tests cover current actor changes after middleware, successful
restored-session controls, intended self operations, whole-transaction failures
and real persisted-result readback. MySQL/PostgreSQL tests cover native waits,
administrator races, token-index busy/retry, issuance competition and real final-
write COMMIT failure. A local LDAP wire fixture covers search and bounded
cancellation; it does not establish acceptance against a business directory.

No deployment or new real administration UI/official RustDesk1.5.0 acceptance
occurred in this slice. The user's subsequent decisions keep existing group and
sharing-recipient deletion behavior for this batch; reference policy moves to
separate design work. The proposed new reject-while-referenced protocol was not
adopted or implemented. Existing collection/primary-book and named-strategy
reference protections already delivered remain in effect.

The legacy pre-login shared-name list keeps its current public behavior: it
returns owner1 collections and their existing metadata/response structure. The
proposal to require login and filter by grants was not adopted for this batch.
Neither proposal remains an unanswered confirmation request. No new model or
runtime behavior is introduced by these decisions. Existing ordinary OIDC login
and basic My address book manual evidence keep their original scope.
