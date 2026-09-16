#!/bin/sh
# Generate/check only control.pb.go using already cached, pinned Go sources.
# Never invokes remote plugins, downloads modules, or cleans gen/go.
set -eu

mode=${1:-check}
case "$mode" in
    check|generate) ;;
    *) echo 'usage: control-proto-offline.sh [check|generate]' >&2; exit 2 ;;
esac

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
module_dir=$(dirname -- "$script_dir")
module_cache=$(GOTOOLCHAIN=local go env GOMODCACHE)
buf_source=$module_cache/github.com/bufbuild/buf@v1.72.0
plugin_source=$module_cache/google.golang.org/protobuf@v1.36.11
if [ ! -f "$buf_source/cmd/buf/buf.go" ] || [ ! -f "$plugin_source/cmd/protoc-gen-go/main.go" ]; then
    echo 'required pinned protobuf generator sources are not cached' >&2
    exit 1
fi

scratch_dir=$(mktemp -d /tmp/execution-control-proto.XXXXXX)
cleanup() {
    case "$scratch_dir" in
        /tmp/execution-control-proto.??????) rm -rf -- "$scratch_dir" ;;
    esac
}
trap cleanup EXIT HUP INT TERM
(
    cd "$buf_source"
    GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOWORK=off go build -o "$scratch_dir/buf" ./cmd/buf
)
(
    cd "$plugin_source"
    GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOWORK=off go build -o "$scratch_dir/protoc-gen-go" ./cmd/protoc-gen-go
)

cd "$module_dir"
template="{\"version\":\"v2\",\"clean\":false,\"plugins\":[{\"local\":\"$scratch_dir/protoc-gen-go\",\"out\":\"gen/go\",\"opt\":[\"paths=source_relative\"]}]}"
"$scratch_dir/buf" generate --template "$template" --path api/proto/execution/v1/control.proto --output "$scratch_dir/output"
generated=$scratch_dir/output/gen/go/execution/v1/control.pb.go
target=$module_dir/gen/go/execution/v1/control.pb.go
test -f "$generated"
if [ "$mode" = generate ]; then
    cp "$generated" "$target"
else
    diff -u "$target" "$generated"
fi
