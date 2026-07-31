#!/bin/sh
set -eu

PURGE=false
case "${1:-}" in
    "") ;;
    --purge) PURGE=true ;;
    --help|-h)
        echo "usage: xui-agent-uninstall [--purge]"
        echo "--purge also removes configuration, identity, and the service user"
        exit 0
        ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
esac

if [ "$(id -u)" -ne 0 ]; then
    echo "xui-agent-uninstall must run as root" >&2
    exit 1
fi

if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    init_system=systemd
elif [ -d /run/openrc ] && command -v rc-service >/dev/null 2>&1 && command -v rc-update >/dev/null 2>&1; then
    init_system=openrc
else
    echo "unsupported init system: refusing to remove a potentially running service" >&2
    exit 1
fi

if [ "$init_system" = systemd ]; then
    systemctl disable --now xui-agent.service xui-agent-xray.path xui-agent-xray.service 2>/dev/null || true
else
    rc-service xui-agent stop 2>/dev/null || true
    rc-service xui-agent-xray stop 2>/dev/null || true
    rc-update del xui-agent default 2>/dev/null || true
    rc-update del xui-agent-xray default 2>/dev/null || true
fi
rm -f /etc/systemd/system/xui-agent.service /etc/systemd/system/xui-agent-xray.path /etc/systemd/system/xui-agent-xray.service
rm -f /etc/init.d/xui-agent /etc/init.d/xui-agent-xray
if [ "$init_system" = systemd ]; then
    systemctl daemon-reload
    systemctl reset-failed xui-agent.service 2>/dev/null || true
fi
rm -f /etc/xui-agent/runtime-assets.sha256
rm -f /usr/local/bin/xui-agent /usr/local/sbin/xui-agent-uninstall
rm -f /usr/local/libexec/xui-agent-launcher /usr/local/libexec/xui-agent-xray-launcher

if [ "$PURGE" = true ]; then
    if [ -f /etc/xui-agent/xray-config-acl.path ] && command -v setfacl >/dev/null 2>&1; then
        xray_config=$(sed -n '1p' /etc/xui-agent/xray-config-acl.path)
        if [ -n "$xray_config" ] && [ -e "$xray_config" ]; then
            setfacl -x u:xui-agent "$xray_config" 2>/dev/null || true
        fi
    fi
    rm -rf /etc/xui-agent /var/lib/xui-agent
    if [ "$init_system" = systemd ]; then
        userdel xui-agent 2>/dev/null || true
        groupdel xui-agent 2>/dev/null || true
    else
        deluser xui-agent 2>/dev/null || true
        delgroup xui-agent 2>/dev/null || true
    fi
fi

echo "xui-agent removed; revoke its node credential in the center if it was not already revoked"
