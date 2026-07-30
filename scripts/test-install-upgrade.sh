#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
BUILD_DIRECTORY=
CENTER_PID=
TEST_USER_CREATED=false
TEST_GROUP_CREATED=false
TEST_LAYOUT_CREATED=false

cleanup() {
    if [ "$TEST_LAYOUT_CREATED" = true ]; then
        systemctl disable --now xui-agent.service xui-agent-xray.path xui-agent-xray.service 2>/dev/null || true
        rm -f /etc/systemd/system/xui-agent.service /etc/systemd/system/xui-agent-xray.path /etc/systemd/system/xui-agent-xray.service
        systemctl daemon-reload 2>/dev/null || true
        rm -f /usr/local/bin/xui-agent /usr/local/sbin/xui-agent-uninstall /usr/local/libexec/xui-agent-launcher
        rm -rf /etc/xui-agent /var/lib/xui-agent
    fi
    if [ "$TEST_USER_CREATED" = true ]; then
        userdel xui-agent 2>/dev/null || true
    fi
    if [ "$TEST_GROUP_CREATED" = true ]; then
        groupdel xui-agent 2>/dev/null || true
    fi
    if [ -n "$CENTER_PID" ]; then
        kill "$CENTER_PID" 2>/dev/null || true
        wait "$CENTER_PID" 2>/dev/null || true
    fi
    if [ -n "$BUILD_DIRECTORY" ]; then
        rm -rf "$BUILD_DIRECTORY"
    fi
}

if [ "$(id -u)" -ne 0 ]; then
    echo "installer integration test must run as root" >&2
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
    /usr/local/sbin/xui-agent-uninstall \
    /usr/local/libexec/xui-agent-launcher \
    /etc/systemd/system/xui-agent.service \
    /etc/systemd/system/xui-agent-xray.path \
    /etc/systemd/system/xui-agent-xray.service
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
for command in curl go systemctl; do
    if ! command -v "$command" >/dev/null 2>&1; then
        echo "$command is required" >&2
        exit 1
    fi
done

BUILD_DIRECTORY=$(mktemp -d)
trap cleanup EXIT HUP INT TERM
GOMAXPROCS=${GOMAXPROCS:-2} go build -o "$BUILD_DIRECTORY/controlserver" ./internal/agent/testdata/controlserver
"$BUILD_DIRECTORY/controlserver" -ready-file "$BUILD_DIRECTORY/center-url" &
CENTER_PID=$!
for _ in $(seq 1 50); do
    if [ -s "$BUILD_DIRECTORY/center-url" ]; then
        break
    fi
    sleep 0.1
done
if [ ! -s "$BUILD_DIRECTORY/center-url" ]; then
    echo "test control server did not start" >&2
    exit 1
fi
CENTER_URL=$(sed -n '1p' "$BUILD_DIRECTORY/center-url")

mkdir -p "$BUILD_DIRECTORY/v0.2.0"
curl --proto '=https' --tlsv1.2 -fsSL \
    https://github.com/qqqasdwx/xui-agent/releases/download/v0.2.0/xui-agent-linux-amd64.tar.gz \
    -o "$BUILD_DIRECTORY/v0.2.0/xui-agent-linux-amd64.tar.gz"
curl --proto '=https' --tlsv1.2 -fsSL \
    https://github.com/qqqasdwx/xui-agent/releases/download/v0.2.0/SHA256SUMS \
    -o "$BUILD_DIRECTORY/v0.2.0/SHA256SUMS"

GOMAXPROCS=${GOMAXPROCS:-2} "$ROOT/scripts/build-release.sh" v0.3.0-test "$BUILD_DIRECTORY/v0.3.0-test"
GOMAXPROCS=${GOMAXPROCS:-2} "$ROOT/scripts/build-release.sh" v0.3.1-test "$BUILD_DIRECTORY/v0.3.1-test"

TEST_LAYOUT_CREATED=true
TEST_USER_CREATED=true
TEST_GROUP_CREATED=true
XUI_AGENT_ENROLLMENT_TOKEN=installer-integration-token \
    "$ROOT/deploy/install.sh" \
    --version v0.2.0 \
    --server-url "$CENTER_URL" \
    --allow-insecure \
    --xray-binary /bin/true \
    --xray-config /nonexistent/xray.json \
    --archive "$BUILD_DIRECTORY/v0.2.0/xui-agent-linux-amd64.tar.gz" \
    --checksums "$BUILD_DIRECTORY/v0.2.0/SHA256SUMS"
if ! getent group xui-agent >/dev/null 2>&1; then
    echo "installer did not create the service group" >&2
    exit 1
fi
if ! id xui-agent >/dev/null 2>&1; then
    echo "installer did not create the service user" >&2
    exit 1
fi

XUI_AGENT_INSTALL_HEALTH_TIMEOUT=30 "$ROOT/deploy/install.sh" \
    --version v0.3.0-test \
    --server-url "$CENTER_URL" \
    --allow-insecure \
    --archive "$BUILD_DIRECTORY/v0.3.0-test/xui-agent-linux-amd64.tar.gz" \
    --checksums "$BUILD_DIRECTORY/v0.3.0-test/SHA256SUMS"
if [ "$(/usr/local/bin/xui-agent run -version)" != v0.3.0-test ]; then
    echo "successful installer upgrade did not activate v0.3.0-test" >&2
    exit 1
fi
if [ -e /var/lib/xui-agent/update-pending.json ]; then
    echo "successful installer upgrade left pending state" >&2
    exit 1
fi
previous_target=$(readlink /var/lib/xui-agent/previous)
case "$previous_target" in
    versions/v0.2.0-*/xui-agent) ;;
    *) echo "successful installer upgrade did not preserve v0.2.0 for rollback" >&2; exit 1 ;;
esac

cp /etc/xui-agent/config.json "$BUILD_DIRECTORY/config.json"
printf '{\n' > /etc/xui-agent/config.json
chown root:xui-agent /etc/xui-agent/config.json
chmod 0640 /etc/xui-agent/config.json
if XUI_AGENT_INSTALL_HEALTH_TIMEOUT=30 "$ROOT/deploy/install.sh" \
    --version v0.3.1-test \
    --server-url "$CENTER_URL" \
    --allow-insecure \
    --archive "$BUILD_DIRECTORY/v0.3.1-test/xui-agent-linux-amd64.tar.gz" \
    --checksums "$BUILD_DIRECTORY/v0.3.1-test/SHA256SUMS"
then
    echo "installer accepted a candidate that could not start" >&2
    exit 1
fi
install -m 0640 -o root -g xui-agent "$BUILD_DIRECTORY/config.json" /etc/xui-agent/config.json
systemctl reset-failed xui-agent.service 2>/dev/null || true
systemctl restart xui-agent.service
if [ "$(/usr/local/bin/xui-agent run -version)" != v0.3.0-test ]; then
    echo "failed installer upgrade did not restore v0.3.0-test" >&2
    exit 1
fi
systemctl --quiet is-active xui-agent.service

echo "installer upgrade and rollback integration test passed"
