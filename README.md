# xui-agent

`xui-agent` is the lightweight node runtime for xui-stack. It initiates an authenticated connection to the central 3x-ui control plane and requires no inbound management port, local panel, Docker daemon, or Docker socket.

## Current Scope

The Agent reports system and Xray status, supports signed updates of its own binary, and can shadow-validate a complete node configuration received from the central control plane. A shadow validation writes only to the Agent state directory and runs the configured Xray binary with `run -test`; it does not replace the running configuration or start, stop, signal, or restart Xray.

Setting `xray.mode` to `managed` enables the separate configuration-apply capability. Managed mode stores immutable configuration versions under the Agent state directory, atomically switches `current.json` and `previous.json`, and runs Xray in a dedicated systemd service. A candidate is confirmed only after the replacement Xray process remains healthy; startup failure restores the previous version. The default `observe` mode never advertises this capability.

Configuration validation commands are limited to 4 MiB, carry a monotonic version and SHA-256 digest, and are idempotent across Agent restarts. Older versions and a reused version with a different digest are rejected.

Existing node panels, Vector agents, and risk processing remain authoritative until their later migration gates are completed.

## Install

Create an enrollment token for an existing node in the central 3x-ui Nodes page. Run the generated installation command as root on that node. The command downloads a release archive, verifies its SHA-256, creates the dedicated `xui-agent` user, enrolls once, and starts the systemd service.

The equivalent command shape is:

```sh
curl -fsSL https://github.com/qqqasdwx/xui-agent/releases/latest/download/install.sh |
  env XUI_AGENT_ENROLLMENT_TOKEN='one-time-token' sh -s -- \
    --server-url 'https://panel.example.com/base-path' \
    --update-public-key 'base64-ed25519-public-key'
```

Plain HTTP is rejected unless `--allow-insecure` is explicitly provided. This option is intended for isolated tests.

The installer preserves an existing configuration and identity. Supplying a new one-time token performs credential rotation without placing the token in a file.

## Runtime Files

- `/etc/xui-agent/config.json`: non-secret runtime configuration, mode `0640`
- `/var/lib/xui-agent/identity.json`: node credential, mode `0600`
- `/var/lib/xui-agent/versions/`: Agent release binaries
- `/var/lib/xui-agent/xray-config/candidate.json`: latest shadow candidate, mode `0600`
- `/var/lib/xui-agent/xray-config/validation.json`: durable validation result, mode `0600`
- `/var/lib/xui-agent/xray-config/versions/`: immutable managed Xray configurations
- `/var/lib/xui-agent/xray-config/current.json`: active managed configuration symlink
- `/var/lib/xui-agent/xray-config/previous.json`: rollback configuration symlink
- `/var/lib/xui-agent/xray-config/applied.json`: confirmed managed configuration state
- `/var/lib/xui-agent/current`: atomically selected Agent binary
- `/var/lib/xui-agent/previous`: rollback target
- `/usr/local/libexec/xui-agent-launcher`: stable systemd launcher

The launcher rolls back a pending release when the new binary exits before completing an authenticated heartbeat. A successful first heartbeat removes the pending marker and confirms the update.

## Commands

```sh
xui-agent run -config /etc/xui-agent/config.json
xui-agent enroll -config /etc/xui-agent/config.json
xui-agent status -config /etc/xui-agent/config.json
xui-agent version
```

`status` never prints the stored node credential.

Stop and remove the runtime while retaining configuration and identity:

```sh
xui-agent-uninstall
```

Remove configuration, identity, versions, and the service user as well:

```sh
xui-agent-uninstall --purge
```

Revoke the node credential in the center before permanent removal.

## Releases

Release tags build deterministic Linux archives for `amd64`, `arm64`, and `armv7`. The workflow requires the GitHub Actions secret `XUI_AGENT_RELEASE_PRIVATE_KEY`, containing a base64 Ed25519 seed or private key. The workflow verifies that its corresponding public key matches [`deploy/release-public-key.txt`](deploy/release-public-key.txt) before publishing and refuses to overwrite existing release assets.

Configure the value from `deploy/release-public-key.txt` on the central 3x-ui process as `XUI_AGENT_RELEASE_PUBLIC_KEY`; enrollment commands then pin that key in the Agent configuration.

Local checks:

```sh
make verify
make release-snapshot
```

On a disposable systemd development VM with no existing xui-agent installation, the managed Xray lifecycle can be tested end to end with:

```sh
make integration-systemd
```

The integration target refuses to run when the `xui-agent` account, units, configuration, state directory, or reserved test binaries already exist. It creates and removes those resources during the test.
