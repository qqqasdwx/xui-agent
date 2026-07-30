#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
BUILD_DIRECTORY=
TEST_USER_CREATED=false
TEST_GROUP_CREATED=false
TEST_LAYOUT_CREATED=false

cleanup() {
    if [ "$TEST_LAYOUT_CREATED" = true ]; then
        systemctl stop xui-agent-xray.path xui-agent-xray.service 2>/dev/null || true
        rm -f /run/systemd/system/xui-agent-xray.path /run/systemd/system/xui-agent-xray.service
        systemctl daemon-reload 2>/dev/null || true
        rm -f /usr/local/bin/xui-agent
        rm -f /usr/local/libexec/xui-agent-test-xray /usr/local/libexec/xui-agent-runtime-integration.test
        rm -rf /etc/xui-agent /var/lib/xui-agent
    fi
    if [ "$TEST_USER_CREATED" = true ]; then
        userdel xui-agent 2>/dev/null || true
    fi
    if [ "$TEST_GROUP_CREATED" = true ]; then
        groupdel xui-agent 2>/dev/null || true
    fi
    if [ -n "$BUILD_DIRECTORY" ]; then
        rm -rf "$BUILD_DIRECTORY"
    fi
}

if [ "$(id -u)" -ne 0 ]; then
    echo "systemd integration test must run as root" >&2
    exit 1
fi
if [ "$(ps -p 1 -o comm=)" != systemd ]; then
    echo "systemd is not PID 1" >&2
    exit 1
fi
for path in \
    /etc/xui-agent \
    /var/lib/xui-agent \
    /usr/local/bin/xui-agent \
    /usr/local/libexec/xui-agent-test-xray \
    /usr/local/libexec/xui-agent-runtime-integration.test \
    /etc/systemd/system/xui-agent-xray.path \
    /etc/systemd/system/xui-agent-xray.service \
    /run/systemd/system/xui-agent-xray.path \
    /run/systemd/system/xui-agent-xray.service
do
    if [ -e "$path" ] || [ -L "$path" ]; then
        echo "refusing to replace existing integration path: $path" >&2
        exit 1
    fi
done
if getent passwd xui-agent >/dev/null 2>&1 || getent group xui-agent >/dev/null 2>&1; then
    echo "refusing to use an existing xui-agent account" >&2
    exit 1
fi

TEST_LAYOUT_CREATED=true
trap cleanup EXIT HUP INT TERM
groupadd --system xui-agent
TEST_GROUP_CREATED=true
useradd --system --gid xui-agent --home-dir /var/lib/xui-agent --shell /usr/sbin/nologin xui-agent
TEST_USER_CREATED=true
install -d -m 0755 /usr/local/libexec
install -d -m 0750 -o root -g xui-agent /etc/xui-agent
install -d -m 0700 -o xui-agent -g xui-agent /var/lib/xui-agent

BUILD_DIRECTORY=$(mktemp -d)
GOMAXPROCS=${GOMAXPROCS:-2} go build -o "$BUILD_DIRECTORY/xui-agent" ./cmd/xui-agent
GOMAXPROCS=${GOMAXPROCS:-2} go build -o "$BUILD_DIRECTORY/fake-xray" ./internal/xrayruntime/testdata/fakexray
GOMAXPROCS=${GOMAXPROCS:-2} go test -p=1 -tags=integration -c -o "$BUILD_DIRECTORY/runtime-integration.test" ./internal/xrayruntime

install -m 0755 "$BUILD_DIRECTORY/xui-agent" /usr/local/bin/xui-agent
install -m 0755 "$BUILD_DIRECTORY/fake-xray" /usr/local/libexec/xui-agent-test-xray
install -m 0755 "$BUILD_DIRECTORY/runtime-integration.test" /usr/local/libexec/xui-agent-runtime-integration.test
install -m 0644 "$ROOT/deploy/xui-agent-xray.service" /run/systemd/system/xui-agent-xray.service
install -m 0644 "$ROOT/deploy/xui-agent-xray.path" /run/systemd/system/xui-agent-xray.path

/usr/local/bin/xui-agent init-config \
    -config /etc/xui-agent/config.json \
    -server-url https://center.integration.invalid \
    -state-directory /var/lib/xui-agent \
    -xray-mode managed \
    -xray-binary /usr/local/libexec/xui-agent-test-xray
chown root:xui-agent /etc/xui-agent/config.json
chmod 0640 /etc/xui-agent/config.json

systemctl daemon-reload
systemctl start xui-agent-xray.path
runuser -u xui-agent -- env \
    XUI_AGENT_SYSTEMD_INTEGRATION=1 \
    XUI_AGENT_INTEGRATION_STATE=/var/lib/xui-agent \
    XUI_AGENT_INTEGRATION_XRAY=/usr/local/libexec/xui-agent-test-xray \
    /usr/local/libexec/xui-agent-runtime-integration.test \
    -test.v -test.run '^TestManagedRuntimeWithSystemd$'
systemctl --quiet is-active xui-agent-xray.path
systemctl --quiet is-active xui-agent-xray.service

echo "systemd integration test passed"
