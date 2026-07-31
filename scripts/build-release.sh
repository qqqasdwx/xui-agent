#!/bin/sh
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
    echo "usage: $0 VERSION [OUTPUT_DIRECTORY]" >&2
    exit 2
fi

VERSION=$1
OUTPUT_DIRECTORY=${2:-dist}
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
case "$OUTPUT_DIRECTORY" in
    /*) ;;
    *) OUTPUT_DIRECTORY="$ROOT/$OUTPUT_DIRECTORY" ;;
esac
case "$OUTPUT_DIRECTORY" in
    /|"$ROOT") echo "refusing unsafe output directory: $OUTPUT_DIRECTORY" >&2; exit 2 ;;
esac

COMMIT=$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || printf 'unknown')
BUILD_DATE=$(git -C "$ROOT" show -s --format=%cI HEAD 2>/dev/null || printf 'unknown')
SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-$(git -C "$ROOT" show -s --format=%ct HEAD 2>/dev/null || date +%s)}

rm -rf "$OUTPUT_DIRECTORY"
mkdir -p "$OUTPUT_DIRECTORY"

build_archive() {
    goarch=$1
    suffix=$2
    goarm=${3:-}
    staging=$(mktemp -d)
    trap 'rm -rf "$staging"' EXIT HUP INT TERM

    if [ -n "$goarm" ]; then
        CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" GOARM="$goarm" \
            go build -trimpath -buildvcs=false \
            -ldflags "-s -w -buildid= -X main.version=$VERSION -X main.commit=$COMMIT -X main.buildDate=$BUILD_DATE" \
            -o "$staging/xui-agent" ./cmd/xui-agent
    else
        CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
            go build -trimpath -buildvcs=false \
            -ldflags "-s -w -buildid= -X main.version=$VERSION -X main.commit=$COMMIT -X main.buildDate=$BUILD_DATE" \
            -o "$staging/xui-agent" ./cmd/xui-agent
    fi
    install -m 0644 "$ROOT/deploy/xui-agent.service" "$staging/xui-agent.service"
    install -m 0644 "$ROOT/deploy/xui-agent-xray.service" "$staging/xui-agent-xray.service"
    install -m 0644 "$ROOT/deploy/xui-agent-xray.path" "$staging/xui-agent-xray.path"
    install -m 0755 "$ROOT/deploy/xui-agent.openrc" "$staging/xui-agent.openrc"
    install -m 0755 "$ROOT/deploy/xui-agent-xray.openrc" "$staging/xui-agent-xray.openrc"
    install -m 0755 "$ROOT/deploy/xui-agent-launcher" "$staging/xui-agent-launcher"
    install -m 0755 "$ROOT/deploy/xui-agent-xray-launcher" "$staging/xui-agent-xray-launcher"
    install -m 0755 "$ROOT/deploy/uninstall.sh" "$staging/uninstall.sh"

    archive="xui-agent-linux-$suffix.tar.gz"
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$SOURCE_DATE_EPOCH" \
        -C "$staging" -czf "$OUTPUT_DIRECTORY/$archive" \
        xui-agent xui-agent-launcher xui-agent-xray-launcher \
        xui-agent.openrc xui-agent-xray.openrc \
        xui-agent.service xui-agent-xray.service xui-agent-xray.path uninstall.sh
    rm -rf "$staging"
    trap - EXIT HUP INT TERM
}

build_archive amd64 amd64

(
    cd "$OUTPUT_DIRECTORY"
    install -m 0755 "$ROOT/deploy/install.sh" install.sh
    sha256sum install.sh xui-agent-linux-*.tar.gz > SHA256SUMS
)
