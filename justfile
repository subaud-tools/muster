# ---- .tools family standard: identical in every family repo ----------------
# `just verify` is exactly what CI runs: this tool's `prepare` (files the build
# needs, e.g. an embedded asset), the family gate (tools-actions go-ci, at the
# version .github/workflows/ci.yml pins), then this tool's `verify-extra`.
# The pre-push hook (lefthook.yml) runs it too, so local and CI never differ.
set shell := ["bash", "-euo", "pipefail", "-c"]

default: verify

verify: prepare gate verify-extra

# Everything: verify plus this tool's slow checks (browser, containers), which
# CI runs as their own required jobs.
verify-all: verify verify-slow

# The family Go gate: gofmt, vet, golangci-lint (family config), race tests,
# cross-build. Fetched once per tools-actions version into ~/.cache.
gate:
    #!/usr/bin/env bash
    set -euo pipefail
    v="$(grep -oE 'go-ci@v[0-9]+\.[0-9]+\.[0-9]+' .github/workflows/ci.yml | head -1 | cut -d@ -f2)"
    f="${XDG_CACHE_HOME:-$HOME/.cache}/tools-actions/$v/go-ci/local.sh"
    [ -f "$f" ] || { mkdir -p "$(dirname "$f")"; curl -fsSL "https://raw.githubusercontent.com/subaud-tools/tools-actions/$v/go-ci/local.sh" -o "$f"; }
    bash "$f"

fmt:
    gofmt -w $(git ls-files '*.go')

# Install the lefthook hooks into this clone's own .git/hooks (once per
# clone). A global core.hooksPath (casebook's recorder) forwards to them;
# plain `lefthook install` refuses to run under one.
hooks:
    #!/usr/bin/env bash
    set -euo pipefail
    d="$(cd "$(git rev-parse --git-common-dir)" && pwd)/hooks"
    git config --local core.hooksPath "$d"
    trap 'git config --local --unset core.hooksPath' EXIT
    lefthook install --force >/dev/null
    echo "lefthook hooks installed in $d"

# ---- muster -----------------------------------------------------------------
# Files the gate needs that are not committed (built before the gate, locally
# and in CI). muster has none.
prepare:

# Slow checks CI runs as their own jobs: the DynamoDB tests (Docker).
verify-slow: verify-dynamo

# Version stamp: cmd/muster, justfile, and .github/workflows/release.yml all
# target the SAME internal/version vars via -ldflags -X, so a local `just
# build`, `just verify`, and a release build report the same thing.
version := `cat VERSION`
commit := `git rev-parse --short HEAD 2>/dev/null || echo none`
date := `date -u +%Y-%m-%d`
ldflags := "-X github.com/subaud-tools/muster/internal/version.version=" + version + " -X github.com/subaud-tools/muster/internal/version.commit=" + commit + " -X github.com/subaud-tools/muster/internal/version.date=" + date

# Build the binary.
build:
    CGO_ENABLED=0 go build -ldflags "{{ ldflags }}" -o bin/muster ./cmd/muster

# Tool-specific checks beyond the gate (CI runs this too): the Lambda
# artifact's build (-tags lambda, the only build that links the AWS SDK; no
# other recipe builds it, so without this the tagged code would rot silently
# until a release tried to ship it), the AWS-free device binary, and the
# Claude Code plugin.
verify-extra: aws-free plugin-valid
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags lambda -o /dev/null ./cmd/muster

# The Claude Code plugin passes Claude Code's own validator. Pinned, and run
# through npx, so CI's runner needs nothing installed and no login. Not
# --strict: the manifest carries no version on purpose (Claude Code then
# versions the plugin by commit), and the validator warns about that.
plugin-valid:
    npx -y @anthropic-ai/claude-code@2.1.309 plugin validate plugin/muster

# Assert the device binary links no AWS code. This is the enforcement half of
# CLAUDE.md's hard rule: cmd/muster-deploy and the -tags lambda build may
# import the AWS SDK, and cmd/muster may NEVER reach either. A stray import in
# internal/daemon or internal/remote would otherwise ship the SDK to every
# device, and nothing else in the build would notice.
aws-free:
    #!/usr/bin/env bash
    set -euo pipefail
    n=$(go list -deps ./cmd/muster | grep -c aws || true)
    if [ "$n" -ne 0 ]; then
      echo "FAIL: cmd/muster links $n AWS package(s) — the device binary must be AWS-free:" >&2
      go list -deps ./cmd/muster | grep aws >&2
      exit 1
    fi
    echo "ok: cmd/muster links no AWS packages"

# DynamoDB backend tests against DynamoDB Local, plus the DynamoDB half of the
# cross-backend conformance suite. Requires Docker, so it is deliberately NOT
# part of `verify`: that gate must stay fast and dependency-free. Without an
# endpoint the dynamo tests skip, so `verify` still compiles and vets them (and
# runs the SQLite half of the conformance suite) — it just can't exercise the
# DynamoDB semantics (conditional writes, atomic counters, transactions) this
# recipe covers. The `dynamo` job in .github/workflows/ci.yml runs the same two
# packages against a service container.
verify-dynamo:
    #!/usr/bin/env bash
    set -euo pipefail
    docker rm -f muster-ddb >/dev/null 2>&1 || true
    docker run -d --rm -p 8000:8000 --name muster-ddb amazon/dynamodb-local >/dev/null
    trap 'docker rm -f muster-ddb >/dev/null 2>&1 || true' EXIT
    for _ in $(seq 1 30); do
      curl -s http://localhost:8000 >/dev/null 2>&1 && break
      sleep 0.5
    done
    MUSTER_DDB_ENDPOINT=http://localhost:8000 go test -race ./internal/dynamostore/... ./internal/storetest/...
