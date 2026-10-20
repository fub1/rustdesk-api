# Collection management rules and delivery scope

The existing `/api/admin/address_book_collection/*` and
`/api/admin/my/address_book_collection/*` routes now use an instance-owned
collection store. Their paths, `api-token` authentication, pagination fields and
legacy `code/message/data` envelopes remain available. No table, column or
database version is added (runtime version 281).

## Confirmed rules

- A collection's owner is fixed after creation. `my/create` belongs to the
  current user; a global administrator can create for an existing enabled user.
  Updates only rename. An omitted, zero or unchanged `user_id` preserves the
  owner; another value is rejected. Request IDs cannot create existing rows;
  submitted timestamps and other ORM metadata cannot change protected fields.
- Only the current owner can rename/delete through `my`. A shared
  full-control grant does not grant collection metadata rights. Administrator
  access can maintain historical owner-zero collections, but cannot create
  another owner-zero collection.
- Any actual `peers.ab_id` reference prevents deletion, including the approved
  rule for deletion of the owning user. No peer, alias or strategy is detached
  to make deletion succeed. An unused `preset_ab_id` is not an active reference.
- An unreferenced collection and its address-book entries, tag definitions and
  sharing rules are deleted by `collection_id` in one checked transaction.
  This also cleans historical child rows with mismatched `user_id`. Other
  collections and personal (`collection_id=0`) scopes remain untouched.
- Existing dangling references are not guessed or repaired.

## Implementation and verification boundary

LEG01 covers collection create/update/delete/list/detail and the corresponding
legacy collection service. Current users, administrator roles, token identity,
stored expiration and signed expiration are rechecked inside the transaction.
The lock order is sorted users, session, collection; delete performs two current
reference counts at READ COMMITTED and never locks peers after the collection.
SQL/COMMIT failures roll back and return safe errors rather than SQL text.

LEG01B extends this rule to deletion of the owning user and the legacy
`PeerService.UpdateAlias` binding writer. User deletion checks actual references
before cleaning any collection; unreferenced collections and their children,
identities, sessions, personal tags and peer ownership are cleaned atomically.
Binding locks the candidate owner, peer and existing collection, then verifies
the owner again. Historical owner-zero collections skip the user lock but
retain the collection lock. Missing targets or ownership drift fail without
changing the peer. The relation has no new native foreign key.

User deletion uses NOWAIT for existing identity and session token groups. If
these are busy, the entire deletion returns a safe unavailable error with no
business changes; retry after the concurrent operation completes. Token groups
use the same token-index access path as session revocation, avoiding the two
MySQL secondary-index/primary-key deadlocks reproduced during verification.
Successful deletion only removes its checked, locked session IDs. Other SQL
and COMMIT failures also roll back without exposing SQL text.

This legacy service signature does not add fresh administrator authorization
to the old user-delete controller. Last-administrator concurrent deletion and
legacy UserThird autocommit late writes are still outside this slice. Legacy
entry/tag/sharing-rule write scopes and atomic entry/peer updates are separate
implementation slices: the old entry controller still commits the entry before
calling the guarded peer writer. The existing anonymous shared-name listing is
unchanged.

## Confirmed next write rules

The `my` entry/tag/sharing-rule writers manage only books owned by the current
user, including personal books. Owner 1 has no special bypass. Administrators
also validate the actual collection owner. Shared grants continue to authorize
client `/api/ab` writes through its existing permission rules.

Ordinary edits preserve the original owner, book and entry's device identity.
Omitted, zero or unchanged ownership/book values preserve the original scope;
another value is rejected. Moving entries uses add-to-target and delete-from-
source. No migration endpoint or model is added.

Tag rename/delete rewrites entry tag arrays only in the same owner/book and
commits with the definitions. Other entry fields remain unchanged. Only an
administrator's entry update retains the peer primary-book/alias/hostname/user
name side effect, in the same transaction. `my` create and administrator batch
add no longer independently change the peer alias. Administrator entry delete
clears primary-book/alias only when that actual primary book matches its book.
These are confirmed rules for the following slices, not completed delivery in
LEG01B.

Automated HTTP tests include persistent revocation, disablement, demotion and
expiration injected after authentication middleware has passed. Core tests
cover ownership drift, cross-instance isolation, SQL failure, actual COMMIT
failure and rollback. MySQL/PostgreSQL results and per-commit CI evidence are
recorded separately in the development plan; do not interpret planned checks
as passed checks.

There is no new deployment or official RustDesk 1.5.0 GUI acceptance for these
management changes. The earlier OIDC login and basic My address book manual
checks remain limited to their original scope. Real legacy administration UI
CRUD, reference rejection and client behavior after management changes remain
unaccepted.
