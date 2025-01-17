#!/bin/bash

set -ex  # Exit on error; debugging enabled.
set -o pipefail  # Fail a pipe if any sub-command fails.

die() {
  echo "$@" >&2
  exit 1
}

PATH="$GOPATH/bin:$GOROOT/bin:$PATH"

# Check proto in manual runs or cron runs.
if [[ "$TRAVIS" != "true" || "$TRAVIS_EVENT_TYPE" = "cron" ]]; then
  check_proto="true"
fi

if [ "$1" = "-install" ]; then
  go get -d \
    github.com/publica-project/grpc/...
  go get -u \
    github.com/golang/lint/golint \
    golang.org/x/tools/cmd/goimports \
    honnef.co/go/tools/cmd/staticcheck \
    github.com/client9/misspell/cmd/misspell \
    github.com/golang/protobuf/protoc-gen-go
  if [[ "$check_proto" = "true" ]]; then
    if [[ "$TRAVIS" = "true" ]]; then
      PROTOBUF_VERSION=3.3.0
      PROTOC_FILENAME=protoc-${PROTOBUF_VERSION}-linux-x86_64.zip
      pushd /home/travis
      wget https://github.com/google/protobuf/releases/download/v${PROTOBUF_VERSION}/${PROTOC_FILENAME}
      unzip ${PROTOC_FILENAME}
      bin/protoc --version
      popd
    elif ! which protoc > /dev/null; then
      die "Please install protoc into your path"
    fi
  fi
  exit 0
elif [[ "$#" -ne 0 ]]; then
  die "Unknown argument(s): $*"
fi

# TODO: Remove this check and the mangling below once "context" is imported
# directly.
if git status --porcelain | read; then
  die "Uncommitted or untracked files found; commit changes first"
fi

git ls-files "*.go" | xargs grep -L "\(Copyright [0-9]\{4,\} gRPC authors\)\|DO NOT EDIT" 2>&1 | tee /dev/stderr | (! read)
gofmt -s -d -l . 2>&1 | tee /dev/stderr | (! read)
goimports -l . 2>&1 | tee /dev/stderr | (! read)
golint ./... 2>&1 | (grep -vE "(_mock|\.pb)\.go:" || true) | tee /dev/stderr | (! read)

# Undo any edits made by this script.
cleanup() {
  git reset --hard HEAD
}
trap cleanup EXIT

# Rewrite golang.org/x/net/context -> context imports (see grpc/grpc-go#1484).
# TODO: Remove this mangling once "context" is imported directly (grpc/grpc-go#711).
git ls-files "*.go" | xargs sed -i 's:"golang.org/x/net/context":"context":'
set +o pipefail
# TODO: Stop filtering pb.go files once golang/protobuf#214 is fixed.
go tool vet -all . 2>&1 | grep -vE '(clientconn|transport\/transport_test).go:.*cancel (function|var)' | grep -vF '.pb.go:' | tee /dev/stderr | (! read)
set -o pipefail
git reset --hard HEAD

if [[ "$check_proto" = "true" ]]; then
  PATH="/home/travis/bin:$PATH" make proto && \
    git status --porcelain 2>&1 | (! read) || \
    (git status; git --no-pager diff; exit 1)
fi

# TODO(menghanl): fix errors in transport_test.
staticcheck -ignore '
github.com/publica-project/grpc/transport/transport_test.go:SA2002
github.com/publica-project/grpc/benchmark/benchmain/main.go:SA1019
github.com/publica-project/grpc/stats/stats_test.go:SA1019
github.com/publica-project/grpc/test/end2end_test.go:SA1019
' ./...
misspell -error .
