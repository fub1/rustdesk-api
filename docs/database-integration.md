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
