# Startup migrations and failure recovery

The database version is recorded **after** schema changes, historical data updates
and initial records succeed. Every database error is returned to the command
entry point. A failed connection or migration exits with status 1 before HTTP
or login/heartbeat background tasks start. The first admin password is reported
only after the seed transaction commits.

SQLite and PostgreSQL run schema, data and version writes in one transaction.
MySQL schema changes run before the data/version transaction because DDL can
implicitly commit. A failed MySQL migration can leave schema changes behind;
the unchanged version does not prove that the entire database is unchanged.

Run only one migrating application instance at a time. This change does not add
a distributed migration lock or a new schema migration framework. MySQL and
PostgreSQL databases must already exist and be reachable; startup does not
request elevated provisioning privileges.

Before upgrading an existing deployment, stop competing API instances and take
a database backup. If startup fails, keep the service stopped, inspect the
returned error and fix its cause. Retry against the same database only after
checking the failed step, especially partial MySQL DDL. If its state is uncertain,
restore the backup and retry the fixed version. Do not manually raise the version
to bypass the failure.

Automated coverage uses real isolated SQLite databases: fresh initialization,
repeated startup, legacy 244/245 OAuth updates, schema conflicts, read errors,
seed failures, version-write failures and successful retries. Child-process
tests verify the actual command exits on connection/migration failure. These
tests do not establish MySQL/PostgreSQL upgrade compatibility; real engine and
deployment samples remain part of the later migration validation work.

Versions 276–278 add the user-approved compatibility models in separate slices:

- 276: `address_books.note`, TEXT NOT NULL. Existing tables first receive a
  nullable column, NULL values are backfilled without touching timestamps, then
  AutoMigrate enforces NOT NULL. No SQL TEXT DEFAULT is required; model-based
  creates explicitly write the empty Go string. Retry also handles an existing
  nullable column left by partial MySQL DDL without replacing non-empty notes.
- 277: `audit_delivery_receipts`, only a digest primary key and payload digest.
  Old audit records do not generate inferred receipts or corrected session IDs.
- 278: `peer_strategy_states`, only the peer primary/foreign key and managed-key
  JSON text. The peer foreign key is installed explicitly despite legacy
  automatic foreign-key migration being disabled. Peer deletion cascades state.
  No previously deleted policy keys can be inferred from legacy rows.

Real SQLite upgrade tests emulate each prior schema, test version-insert failure
rollback and successful retries, repeated startup, and constraints. A nullable
note-column fixture exercises retry state, not actual MySQL DDL behavior.
MySQL/PostgreSQL upgrade, isolation and concurrent behavior still require engine
tests with deployment samples before production acceptance.
