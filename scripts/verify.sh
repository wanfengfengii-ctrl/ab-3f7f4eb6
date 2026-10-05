#!/bin/sh
# One-shot verification, run by the "verify" compose service once the
# application container is healthy:
#   1. build check        (go build ./...)
#   2. static build check (go vet ./...)
#   3. code tests         (go test ./...)
#   4. API smoke test: publish, then download in segments and reassemble
# Any failure aborts the script with a non-zero exit code.
set -eu

cd /src

echo "==> [1/4] build check: go build ./..."
go build ./...

echo "==> [2/4] build check: go vet ./..."
go vet ./...

echo "==> [3/4] code tests: go test ./..."
go test ./...

echo "==> [4/4] API smoke test against ${APP_URL:-http://app:8080}"
exec /usr/local/bin/smoke
