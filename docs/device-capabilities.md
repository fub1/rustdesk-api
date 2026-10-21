# Device API scope and visibility

2026-10-21. This batch implements DEV04 and delivers DEV01/DEV03 contract and
backend-dependency design. Other unfinished work is removed from the active
backlog; historical limitations are not reclassified as passed acceptance.

## DEV04: client directories

The existing authenticated GET routes are `/api/users`, `/api/peers` and
`/api/device-group/accessible`. The user approved these rules without new models:

| Actor | Users | Registered peers | Device groups |
| --- | --- | --- | --- |
| Enabled ordinary user | Self and enabled members of the actor's real shared user group | Devices owned by those visible enabled users, both online and offline | Existing groups referenced by those same visible devices |
| Enabled administrator | All real users across the site | Devices belonging to real users across the site | All existing definitions, including unused groups |

A zero, missing or nonshared user group grants an ordinary user only self scope.
Address-book sharing does not expand these MyGroup directories. Devices with
zero/missing owners remain outside the client directory; existing administrative
device management is unaffected.

`users.group_id` references user groups; `peers.group_id` references device
groups. Equal IDs or names do not create access. A visible peer with no real
device group remains visible with an empty group name, but produces no group
row. Group rows are deduplicated by definition, not by name; no new uniqueness
or historical repair policy is introduced.

Permissions filter before total counting and pagination. Empty directories
return HTTP200 with `total:0,data:[]`; ordinary group-directory access no longer
requires an administrator role. Existing paging parameters and DTOs remain.

For users/peers, `status=1` retains the enabled-account filter; omitted status or
`status=0` adds no optional enabled filter. Ordinary visibility already excludes
disabled accounts and their devices. Other/duplicate values retain rejection.
This parameter filters account state, not device online presence; the peer
DTO's existing status projection does not become an online-state claim.

The transaction reads the current actor role/state/group through the existing
Store boundary. Request authentication remains required; revocation after
admission does not promise cancellation of an in-flight response. The existing
DTOs do not gain credentials, UUIDs, IPs, password hashes, internal remarks or
strategy objects. Runtime schema remains281.

## DEV01: deployment design

The fixed client sends operator-supplied Bearer credentials and JSON strings
`id,uuid,pk` to `POST /api/devices/deploy`. UUID is base64 machine identity bytes,
not necessarily a 16-byte UUID; pk is a base64 Ed25519 public key. The result
enum is `OK`, `NOT_ENABLED`, `INVALID_INPUT` or `ID_TAKEN`; OK triggers a client
registration retry.

The inspected OSS hbbs registers devices automatically, using independent
persistent data and an in-memory peer cache, without an explicit deployment
admission consumer. API metadata writes do not change that behavior; directly
editing hbbs SQLite bypasses its cache and is not a registration API.

The user confirmed API-only scope. DEV01 therefore delivers contract, trust and
backend-dependency design, without a success endpoint, negative-only placeholder
or new table. A future implementation needs an authoritative atomic registration
backend, identity conflict/idempotency rules and actual hbbs admission/cache
integration. These dependencies are not authorized implementation in this batch.

## DEV03: switch-grant design

The fixed client sends `id,switch_code_verifier,timestamp,signature` to
`POST /api/switch-grant`, without Bearer credentials or a public key. It signs
using its device Ed25519 key, expects mandatory `accepted`, and can retry once
using `server_time` to correct its signed timestamp.

The API has no trusted device public-key source, and the inspected OSS hbbs does
not consume switch codes or grant registrations. Anonymous device identifiers
cannot establish trust; storing a verifier without a consumer does not complete
authorization. This batch provides contract/signing/dependency design only, with
no endpoint, success response or model. Pure signing-helper tests do not prove
implemented authorization, replay prevention or real-client acceptance.

The client's local ten-second switch-code lifetime does not specify an API clock
window or retention period. Asynchronous registration and repeated punch/relay
attempts must be addressed before defining atomic consumption.

## Validation boundary

DEV04 uses production-route permission/paging tests and SQLite plus real
MySQL/PostgreSQL checks of the changed queries, dependency failures, cancellation
and current actor facts. Real RustDesk1.5.0 MyGroup/device-group selection
acceptance is recorded separately; automatic HTTP/DB evidence is not GUI evidence.

Baseline: RustDesk1.5.0 `fada664df7a294d1d1a9ca3e7cd3637069122f17`,
hbb_common `229b904508364c8997aad0fb5af57effac859f60`.
See [client contracts](client-api-1.5.0.md) and the already delivered
[CLI contract](device-cli.md).
