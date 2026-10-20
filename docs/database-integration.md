# Real database validation

`internal/integration` uses the production MySQL/PostgreSQL connection factories
and migration entry point. Every engine subtest creates a random `rd_it_` database,
opens independent pools as needed, closes them and drops only that generated DB.
The database named in the supplied admin DSN is never migrated or cleared.
The test account needs CREATE/DROP DATABASE; connection-failure tests additionally
need permission to terminate their own test connections. Never print DSNs.

```sh
export RUSTDESK_TEST_MYSQL_DSN='test-admin:password@tcp(127.0.0.1:3306)/mysql?parseTime=true&loc=UTC&timeout=10s&readTimeout=30s&writeTimeout=30s'
export RUSTDESK_TEST_POSTGRES_DSN='postgres://test-admin:password@127.0.0.1:5432/postgres?sslmode=disable&connect_timeout=10'
export RUSTDESK_TEST_DB_REQUIRED=1
go test -mod=readonly -race -count=1 -shuffle=on -timeout=12m -v ./internal/integration
```

Without a DSN, its local engine tests skip. Required mode fails if either DSN is
missing; a supplied but unreachable engine always fails. CI supplies both real
services (MySQL 8.4 and PostgreSQL 18), enables required mode and runs the race
detector. Validation artifact generation depends on this job succeeding.
Local validation additionally covers Ubuntu MySQL 8.0.46 and PostgreSQL 18.4;
these are tested versions, not a production engine-version recommendation.

Migration coverage includes fresh initialization and repeated startup, an old
275 schema missing the note and delivery/state tables, 276/277 upgrade samples,
legacy 244 OAuth data without inferred OIDC links, note backfill and NOT NULL,
existing data preservation, peer FK/cascade, and a real CHECK violation on the
final version write. PostgreSQL must roll back transactional DDL; MySQL must
retain partial DDL without publishing a version and successfully retry.
These generated historical fixtures are not customer database backups.

Transaction/concurrency cases in this directory exercise the stores using
independent pools. PostgreSQL deferred constraints and MySQL server-side
connection termination distinguish actual commit failure from a mocked error.
Each added case records its scope; these tests do not establish every possible
isolation anomaly, production topology or deployment upgrade procedure.

## Transaction and concurrency scope

The real-engine tests cover audit receipts across 12 independent pools,
different-payload conflicts, replay after business deletion, and actual failed
COMMIT followed by fresh-pool retry. Identity tests cover failed admission
COMMIT with no leaked account/session, concurrent first admission, two users
competing to bind one identity, and byte-exact issuer/subject uniqueness.
Session renewal/revocation run concurrently rather than in a sequential loop.
Policy tests distinguish original serialization/deadlock SQL errors from other
storage errors and verify durable history and clearing after a retry.

A deterministic barrier reproduced a user/session lock inversion between web
issuance and explicit binding on both engines (MySQL 1213 / PostgreSQL 40P01).
Actor checks now lock the user before its session, matching issuance and
password-change order. The same competing operations must both commit after the
fix. This does not prove all cross-provider or legacy operations deadlock-free.
The connection-loss injection happens before COMMIT, not after a confirmed
server commit whose response was lost; ambiguous outcomes remain a separate
boundary.

## Named definitions (PRE02-C / version280)

Only the approved id/name/config_options table is added; there is no peer relation. Names bind as binary values (MySQL VARBINARY255, PostgreSQL BYTEA, SQLite BLOB), preserving UTF-8 byte identity across case, accents, trailing spaces and NUL. The dedicated migration stays outside AutoMigrate to prevent later collation/type regression. Existing invalid shapes fail migration without publishing280 rather than being silently accepted. MySQL requires InnoDB and an unsigned single-column auto-increment primary key.

The real-engine tests cover fresh/upgrade280, failed version persistence and retry, a later281 test target retaining the binary namespace, non-NULL/unique constraints, stale administrator/session rejection, concurrent same-name creation, rename conflict rollback, and INSERT/UPDATE/DELETE actual COMMIT failure with retry. PostgreSQL uses deferred constraint-trigger exceptions; MySQL kills the transaction connection after its final successful DML. These failures occur before a confirmed server COMMIT; lost replies after successful COMMIT remain ambiguous. Physical DDL may remain after a MySQL upgrade failure; the version must not advance until retry succeeds. No production backup/restore or real client acceptance is implied.
