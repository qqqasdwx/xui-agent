#!/bin/sh
set -eu

REPOSITORY=${XUI_AGENT_REPOSITORY:-qqqasdwx/xui-agent}
VERSION=${XUI_AGENT_VERSION:-latest}
SERVER_URL=${XUI_AGENT_SERVER_URL:-}
SERVER_CERT_SHA256=${XUI_AGENT_SERVER_CERT_SHA256:-}
UPDATE_PUBLIC_KEY=${XUI_AGENT_UPDATE_PUBLIC_KEY:-}
ALLOW_INSECURE=false
XRAY_BINARY=${XUI_AGENT_XRAY_BINARY:-}
XRAY_CONFIG=${XUI_AGENT_XRAY_CONFIG:-/usr/local/x-ui/bin/config.json}
XRAY_PID_FILE=${XUI_AGENT_XRAY_PID_FILE:-}
CONFIG_PATH=/etc/xui-agent/config.json
STATE_DIRECTORY=/var/lib/xui-agent
ARCHIVE_PATH=
CHECKSUMS_PATH=

usage() {
    cat <<'EOF'
Usage: install.sh --server-url URL [options]

Options:
  --version VERSION              GitHub release tag (default: latest)
  --server-cert-sha256 DIGEST    Pin the center certificate SHA-256
  --update-public-key KEY        Base64 Ed25519 release verification key
  --allow-insecure               Allow plain HTTP; intended only for tests
  --xray-binary PATH             Existing Xray executable to observe
  --xray-config PATH             Existing Xray config to hash
  --xray-pid-file PATH           Optional existing Xray PID file
  --archive PATH                 Install a local release archive
  --checksums PATH               SHA256SUMS for a local archive
  --help                         Show this help

Set XUI_AGENT_ENROLLMENT_TOKEN for first enrollment or credential rotation.
EOF
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version) VERSION=${2:?missing value for --version}; shift 2 ;;
        --server-url) SERVER_URL=${2:?missing value for --server-url}; shift 2 ;;
        --server-cert-sha256) SERVER_CERT_SHA256=${2:?missing value for --server-cert-sha256}; shift 2 ;;
        --update-public-key) UPDATE_PUBLIC_KEY=${2:?missing value for --update-public-key}; shift 2 ;;
        --allow-insecure) ALLOW_INSECURE=true; shift ;;
        --xray-binary) XRAY_BINARY=${2:?missing value for --xray-binary}; shift 2 ;;
        --xray-config) XRAY_CONFIG=${2:?missing value for --xray-config}; shift 2 ;;
        --xray-pid-file) XRAY_PID_FILE=${2:?missing value for --xray-pid-file}; shift 2 ;;
        --archive) ARCHIVE_PATH=${2:?missing value for --archive}; shift 2 ;;
        --checksums) CHECKSUMS_PATH=${2:?missing value for --checksums}; shift 2 ;;
        --help|-h) usage; exit 0 ;;
        *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done

if [ "$(id -u)" -ne 0 ]; then
    echo "install.sh must run as root" >&2
    exit 1
fi
if [ -z "$SERVER_URL" ]; then
    echo "--server-url is required" >&2
    exit 2
fi
for command in getent groupadd useradd runuser sha256sum systemctl tar; do
    if ! command -v "$command" >/dev/null 2>&1; then
        echo "$command is required" >&2
        exit 1
    fi
done

machine=$(uname -m)
case "$machine" in
    x86_64|amd64) archive_arch=amd64; xray_arch=amd64 ;;
    aarch64|arm64) archive_arch=arm64; xray_arch=arm64 ;;
    armv7l|armv7) archive_arch=armv7; xray_arch=arm32 ;;
    *) echo "unsupported architecture: $machine" >&2; exit 1 ;;
esac
if [ -z "$XRAY_BINARY" ]; then
    XRAY_BINARY="/usr/local/x-ui/bin/xray-linux-$xray_arch"
fi

temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
archive_name="xui-agent-linux-$archive_arch.tar.gz"

if [ -n "$ARCHIVE_PATH" ]; then
    if [ -z "$CHECKSUMS_PATH" ]; then
        echo "--checksums is required with --archive" >&2
        exit 2
    fi
    cp "$ARCHIVE_PATH" "$temporary/$archive_name"
    cp "$CHECKSUMS_PATH" "$temporary/SHA256SUMS"
else
    if ! command -v curl >/dev/null 2>&1; then
        echo "curl is required for a GitHub release install" >&2
        exit 1
    fi
    if [ "$VERSION" = latest ]; then
        release_url="https://github.com/$REPOSITORY/releases/latest/download"
    else
        release_url="https://github.com/$REPOSITORY/releases/download/$VERSION"
    fi
    curl --proto '=https' --tlsv1.2 -fsSL "$release_url/$archive_name" -o "$temporary/$archive_name"
    curl --proto '=https' --tlsv1.2 -fsSL "$release_url/SHA256SUMS" -o "$temporary/SHA256SUMS"
fi

expected=$(awk -v name="$archive_name" '$2 == name { print $1 }' "$temporary/SHA256SUMS")
if [ "${#expected}" -ne 64 ] || printf '%s' "$expected" | grep -q '[^0-9a-fA-F]'; then
    echo "checksum for $archive_name is missing or invalid" >&2
    exit 1
fi
actual=$(sha256sum "$temporary/$archive_name" | awk '{print $1}')
if [ "$actual" != "$expected" ]; then
    echo "release archive checksum mismatch" >&2
    exit 1
fi

entries=$(tar -tzf "$temporary/$archive_name" | LC_ALL=C sort)
expected_entries=$(printf '%s\n' uninstall.sh xui-agent xui-agent-launcher xui-agent.service | LC_ALL=C sort)
if [ "$entries" != "$expected_entries" ]; then
    echo "release archive contains unexpected files" >&2
    exit 1
fi
tar -xzf "$temporary/$archive_name" -C "$temporary"

if ! getent group xui-agent >/dev/null 2>&1; then
    groupadd --system xui-agent
fi
if ! id xui-agent >/dev/null 2>&1; then
    useradd --system --gid xui-agent --home-dir "$STATE_DIRECTORY" --shell /usr/sbin/nologin xui-agent
fi
install -d -m 0750 -o root -g xui-agent /etc/xui-agent
install -d -m 0700 -o xui-agent -g xui-agent "$STATE_DIRECTORY"
install -d -m 0700 -o xui-agent -g xui-agent "$STATE_DIRECTORY/versions"
install -d -m 0755 -o root -g root /usr/local/libexec
install -m 0755 "$temporary/xui-agent-launcher" /usr/local/libexec/xui-agent-launcher
install -m 0644 "$temporary/xui-agent.service" /etc/systemd/system/xui-agent.service
install -m 0755 "$temporary/uninstall.sh" /usr/local/sbin/xui-agent-uninstall

if [ ! -L "$STATE_DIRECTORY/current" ]; then
    bootstrap_digest=$(sha256sum "$temporary/xui-agent" | awk '{print substr($1, 1, 16)}')
    bootstrap_directory="$STATE_DIRECTORY/versions/bootstrap-$bootstrap_digest"
    install -d -m 0700 -o xui-agent -g xui-agent "$bootstrap_directory"
    install -m 0755 -o xui-agent -g xui-agent "$temporary/xui-agent" "$bootstrap_directory/xui-agent"
    runuser -u xui-agent -- ln -s "versions/bootstrap-$bootstrap_digest/xui-agent" "$STATE_DIRECTORY/current"
fi
ln -sfn "$STATE_DIRECTORY/current" /usr/local/bin/xui-agent

if [ ! -f "$CONFIG_PATH" ]; then
    set -- init-config \
        --config "$CONFIG_PATH" \
        --server-url "$SERVER_URL" \
        --state-directory "$STATE_DIRECTORY" \
        --server-cert-sha256 "$SERVER_CERT_SHA256" \
        --update-public-key "$UPDATE_PUBLIC_KEY" \
        --xray-binary "$XRAY_BINARY" \
        --xray-config "$XRAY_CONFIG" \
        --xray-pid-file "$XRAY_PID_FILE"
    if [ "$ALLOW_INSECURE" = true ]; then
        set -- "$@" --allow-insecure
    fi
    /usr/local/bin/xui-agent "$@"
fi
chown root:xui-agent "$CONFIG_PATH"
chmod 0640 "$CONFIG_PATH"

if [ -f "$XRAY_CONFIG" ] && command -v setfacl >/dev/null 2>&1; then
    setfacl -m u:xui-agent:r "$XRAY_CONFIG"
    printf '%s\n' "$XRAY_CONFIG" > /etc/xui-agent/xray-config-acl.path
    chown root:xui-agent /etc/xui-agent/xray-config-acl.path
    chmod 0640 /etc/xui-agent/xray-config-acl.path
fi

if [ ! -f "$STATE_DIRECTORY/identity.json" ] && [ -z "${XUI_AGENT_ENROLLMENT_TOKEN:-}" ]; then
    echo "XUI_AGENT_ENROLLMENT_TOKEN is required for first enrollment" >&2
    exit 2
fi
if [ -n "${XUI_AGENT_ENROLLMENT_TOKEN:-}" ]; then
    runuser -u xui-agent -- env XUI_AGENT_ENROLLMENT_TOKEN="$XUI_AGENT_ENROLLMENT_TOKEN" \
        /usr/local/bin/xui-agent enroll --config "$CONFIG_PATH"
fi

systemctl daemon-reload
systemctl enable xui-agent.service
systemctl restart xui-agent.service
systemctl --quiet is-active xui-agent.service
echo "xui-agent installed and running"
