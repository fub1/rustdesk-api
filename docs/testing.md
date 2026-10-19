# Testing and validation

Use **Go 1.23.10** and a C compiler with `CGO_ENABLED=1`. SQLite uses CGO;
setting `CGO_ENABLED=0` is not equivalent to validating the deployed application.
On Ubuntu, GCC is available on the GitHub-hosted runner. On Windows, use a
compatible GCC toolchain or run the commands inside WSL with Linux Go and GCC.

Use the committed `go.mod` and `go.sum` without updating them while validating:

```sh
export GOTOOLCHAIN=local
export CGO_ENABLED=1
export GOFLAGS=-mod=readonly

go test -count=1 -shuffle=on ./...
go test -race -count=1 -shuffle=on ./...
go vet ./...
go build ./...
mkdir -p build
go build -trimpath -o build/rustdesk-api ./cmd
git diff --exit-code -- go.mod go.sum
```

Dependency changes belong in an intentional maintenance change, followed by review of both manifests.
A missing checksum or a different installed toolchain should fail validation,
not silently edit the dependency graph or select a different Go version.

## Local isolation

File-cache tests and benchmarks use a separate `testing.TB.TempDir()` for every
invocation. Each read prepares its own data, so tests do not depend on order or
leave cache files in a developer's shared temporary directory. Memory-cache
tests stop their background eviction goroutine during cleanup and assert expiry
and LRU results. The keyed-lock tests keep shared counter access inside the lock
and verify that the same key protects every goroutine.

## Redis integration tests

The default local test run **skips Redis integration tests** unless
`RUSTDESK_TEST_REDIS_ADDR` is set. It does not contact a fixed network address.
Use `-v` to see each skip and its opt-in instruction:

```sh
go test -count=1 -v ./lib/cache -run '^TestRedis'
```

To exercise Redis, provide a disposable local Redis 7 server. For example:

```sh
docker run --rm -d --name rustdesk-api-test-redis -p 127.0.0.1:16379:6379 redis:7-alpine
export RUSTDESK_TEST_REDIS_ADDR=127.0.0.1:16379
export RUSTDESK_TEST_REDIS_REQUIRED=1
go test -count=1 -v ./lib/cache -run '^TestRedis'
go test -race -count=1 -shuffle=on ./...
docker stop rustdesk-api-test-redis
```

The Docker example creates a disposable container and stops it explicitly after
testing. On PowerShell, set variables using `$env:NAME='value'` instead of
`export NAME=value`. If the test server requires authentication, set
`RUSTDESK_TEST_REDIS_PASSWORD` in the environment; do not commit credentials.

When `RUSTDESK_TEST_REDIS_REQUIRED=1`, a missing address is a failure. Once an
address is supplied, an unavailable server is always a failure, never a skip.
Each test and benchmark uses its own 128-bit random key prefix in Redis DB 0,
prepares its own data, removes only that key during cleanup, and closes its
connection. Tests never call `FLUSHDB` or `FLUSHALL`. Failed or killed processes
can leave keys until their TTL expires; use a disposable instance, not a
production Redis database. Both the cache factory wrapper and direct Redis-cache
constructor are covered.

## GitHub Actions

`.github/workflows/validate.yml` runs on pushes and pull requests with
`contents: read`. It uses Go 1.23.10, CGO, a healthy Redis service container on a
random localhost port, and requires Redis integration coverage. It runs all
packages with `-race`, `-count=1`, and shuffled order, then shows the Redis tests
in a separate verbose step, runs vet, and builds all packages. There are no
package filters or race-test exclusions.

The binary artifact is built only after those checks from the same checked-out
`github.sha`. It contains the binary, `REVISION`, and `GO_VERSION`, with a SHA-256
checksum and a seven-day artifact retention period. For pull requests,
`github.sha` is the tested merge revision, not necessarily the contributor's
branch head. This is a **validation artifact**: no release, container push,
deployment, repository dispatch, or remote service changes occur. Existing
legacy tag/manual publishing workflows are separate and are not invoked by this
validation workflow.

