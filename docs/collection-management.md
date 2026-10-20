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
legacy UserThird autocommit late writes are still outside this slice. Entry,
tag and sharing-rule writes use the instance stores described below. The
existing anonymous shared-name listing is unchanged.

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
Tag and sharing-rule writers implement these rules in LEG02-C, individual
entry writes in LEG02-D, and the four batch routes in LEG02-E.

## LEG02-C tag and sharing-rule writes

The existing twelve admin/my Tag/Rule write routes use the instance-owned
`bookadmin.Store`. They check the current enabled actor, administrator role,
session PK, raw-token ambiguity and both expirations in the transaction. All
participating users are locked in ascending order before session, parent and
child rows. Shared full-control is not used as management authorization.

Updates preserve the original owner and collection. An administrator may omit
the owner when creating under a collection; its actual nonzero existing owner
is used. New owner-zero children are rejected, while historical owner-zero tags
remain maintainable by administrators. New sharing rules still require a real
collection; historical personal-scope sharing rules can be deleted. Recipient
existence, rule type and permission bounds retain their existing checks; this
slice does not redesign recipient deletion.

Tag names compare by exact bytes. Rename/delete operates on all historical
same-name definitions in the exact owner/book and rewrites participating entry
arrays in the same transaction. An update's color changes only its selected
definition. The legacy create route still tolerates duplicates. Reference
rewrites change only the tags column, preserving notes, credentials, metadata
and entry timestamps. NULL, JSON null, malformed or non-string-array historical
storage fails closed and rolls back; it is not silently converted to an empty
array. New definitions do not introduce another table or constraint.

LEG02-C does not itself cover entry or batch routes. Individual entry routes
join the protocol in LEG02-D below, and batches in LEG02-E. Real web
administration UI and official-client acceptance are still pending.

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

## LEG02-D individual entry writes

The six existing administrator/my create, update and delete routes use the same
instance store and transaction admission checks. Ordinary edits preserve the
stored owner, collection and byte-exact device identity; omitted, zero or
unchanged selectors preserve their values. Only the selected entry row changes.
Entry notes remain independent and are not overwritten by these legacy forms.
Other editable fields retain the existing full-form replacement semantics,
including explicit zero values. New entries store an empty tag array when tags
are absent. Existing duplicate rows are not merged or cleaned up.

Only an administrator's update can also set a registered peer's primary book,
alias, hostname and username. The candidate peer owner is locked before peers
and collections; row ID, device ID, UUID and ownership are rechecked. Missing
peers leave the operation as an entry-only update; ambiguous or changed peers
fail closed. Both writes commit together. The create routes and all my routes
do not independently change peers.

An administrator's delete clears only primary-book and alias, and only when a
nonzero collection is that peer's actual primary book. Other primary books and
peer settings are preserved. Normal ORM update timestamps still apply. Failed
SQL, cancellation and actual COMMIT failures roll back entry and peer changes;
an explicit retry is a new transaction.

Automated checks cover post-middleware session revocation on all six routes,
valid restored-session controls with resource readback, immutable selectors,
peer side effects and rollback. Native MySQL/PostgreSQL checks also exercise
both collection-delete orders, peer-owner deletion and tag-rename competition,
identity/owner drift, and real final-write COMMIT failure followed by retry.
These checks do not constitute real web UI or official-client acceptance.

## LEG02-E batch writes

The existing administrator batch-create, administrator/my from-peers and my
batch-tags routes use the same transaction admission and sorted lock protocol.
Every target is checked before the first business mutation; one invalid or
changed target rejects the whole request. Failed SQL, cancellation or actual
COMMIT failure rolls back all items. No batch changes registered peer settings
or deletes entries in other books.

Administrator batch-create keeps its legacy raw multiple-user behavior: more
than one supplied user ID forces personal-book scope and empty tags, even when
the raw list repeats the same user. Every ID must be nonzero and exist; writes
use sorted unique owners. A single target's collection must have that actual
owner. Only the first existing exact-scope/device row in row-ID order receives
the legacy nonzero-field patch: empty strings and false preserve their values,
but tags are always replaced. Notes, identity, scope and other duplicate rows
are preserved. Missing entries are inserted. The scalar user_id does not
override the user_ids target list.

From-peers requires all requested registered peers, rejecting empty, zero,
duplicate or missing IDs and empty source device identities. My mode requires
current-user-owned sources and destination; administrator mode checks the actual
destination owner and collection. A personal administrator destination requires
an explicit nonzero owner. Source row ID, device ID, UUID and owner are rechecked
after locking. New entries copy only device ID, username, hostname and the
existing OS-to-platform mapping; alias, notes and credentials are not copied.
An existing exact-scope/device entry is skipped entirely.

My batch-tags canonicalizes duplicate row IDs and requires every row to exist
and belong to the current owner and a valid book scope. It replaces only tags
and normal update timestamps; absent/empty tags clear to an empty array. It
does not create tag definitions. The former 999-row truncation is removed;
automated checks request and read back 1001 entries in one operation. There is
no replacement arbitrary row cap.

Native MySQL/PostgreSQL tests cover whole-batch rollback after a later item,
final-write COMMIT failure and retry, parent/source drift, fresh session checks
after a native lock wait, and both orders against collection deletion and
client tag rename. Automated HTTP tests cover all four routes with revoked
sessions and restored-session success controls. No new schema is introduced.
Real administration UI and official-client acceptance remain pending.
