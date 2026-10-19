# Process lifecycle (ARCH 06)

The command owns its resources. Configuration/log-open errors return normally;
database/migration failure releases opened connections and never starts serving.
Migrations receive the command context. Redis/cache resources are created only
after migration, and only for the server command. Maintenance commands do not
start cache eviction, HTTP, the login limiter or heartbeat flushing.

`http.NewHandler` still constructs only a handler. `http.RunContext` now uses
one standard `net/http.Server` on all platforms. It binds the listener before
starting owned workers, then serves; a listen or worker-start failure never
reports readiness and unwinds what started. It replaces the old Unix endless
restart listener and Windows Gin.Run path. SIGINT/SIGTERM initiate shutdown;
Unix signal behavior is tested. There is no inherited-listener/SIGHUP restart
or Windows Service Control Manager integration in this implementation.

The default total shutdown budget is 15 seconds. HTTP gets up to the first
10 seconds to stop accepting and drain active handlers; the remaining budget
is available for worker cancellation/join and final flush. Request contexts
remain live during normal drain. On HTTP timeout, the server cancels request
contexts and closes remaining connections. It returns `ErrIncompleteShutdown`;
the command exits nonzero without closing/resetting dependencies that residual
handlers might still use. Go cannot forcibly stop an arbitrary uncooperative
handler or driver function; process exit ends that residual work. Such an exit
is explicitly not a completed graceful drain.

During normal drain, workers stop before owned cache/Redis/SQL connections and
the log file close. OIDC idle transports and the login-limiter cleanup goroutine
are closed by the server command too. Memory-cache Close is idempotent and joins
eviction. The logger owns its file and does not mutate the process-wide Logrus
standard logger. There is no goroutine spawned per operation-log write: its
bounded five-second best-effort write stays inside the HTTP handler and hence
inside the drain. It ignores client cancellation after the action, but does not
extend its five-second budget. A failed write emits only a static warning; it
does not change an already committed business response or claim durable audit.

Heartbeat buffering is instance-owned. Every ten seconds a transaction writes
the latest observed timestamp/IP per peer, without regressing a newer stored
timestamp. A failed/rolled-back/failed-commit batch merges back into pending
state, preserving newer concurrent observations. Stop rejects new buffers,
cancels/joins periodic work, then attempts a final flush with the remaining
shutdown budget and a five-second per-operation cap. A failure returns an error
and records the pending count. This telemetry is not a durable queue: process
crash or failed final flush can lose pending observations, and heartbeat ACK
does not promise that telemetry has already committed. No worker is created per
peer or per heartbeat.

Verification includes loopback HTTP request drain/forced timeout, worker-start
unwind, real SQLite atomic failure/retry and final flush, concurrent cache/worker
close, operation-log cancellation, and child-process maintenance/SIGTERM tests.
SQLite startup/upgrade faults are covered separately. Windows is compile-only;
real MySQL/PostgreSQL shutdown, driver-native cancellation latency and an actual
RustDesk/Flutter process remain unverified. Forced OS kill cannot flush memory.
