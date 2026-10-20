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

The relation has no native foreign key. The reference guard requires every
binding writer to participate in the same owner/collection protocol; the guard
alone cannot prevent an unrelated late writer. User deletion and the legacy
primary-book writer still require the next implementation slice before the
end-to-end reference rule can be marked complete. Legacy entry/tag/sharing-rule
write scopes are a separate rule and implementation slice. The existing
anonymous shared-name listing is unchanged.

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
