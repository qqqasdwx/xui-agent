# xui-agent

`xui-agent` is the lightweight node runtime for xui-stack. It initiates an authenticated connection to the central 3x-ui control plane and requires no inbound management port, local panel, Docker daemon, or Docker socket.

## Current Scope

The Agent reports system and Xray status, updates its own binary from the fixed `qqqasdwx/xui-agent` GitHub Release source, and can shadow-validate a complete node configuration received from the central control plane. A shadow validation writes only to the Agent state directory and runs the configured Xray binary with `run -test`; it does not replace the running configuration or start, stop, signal, or restart Xray.

Setting `xray.mode` to `managed` enables the separate configuration-apply capability. Managed mode stores immutable configuration versions under the Agent state directory, atomically switches `current.json` and `previous.json`, and runs Xray in a dedicated systemd service. A candidate is confirmed only after the replacement Xray process remains healthy; startup failure restores the previous version. The default `observe` mode never advertises this capability.

Managed mode can also install and update a complete Xray bundle from the fixed `qqqasdwx/Xray-core` GitHub Release source. The executable, `geoip.dat`, and `geosite.dat` are pinned and rolled back as one unit. An existing managed installation may keep its configured Xray path as the bootstrap fallback for the first managed bundle; after that bundle is confirmed, the Agent uses only the selected managed runtime. Existing configurations are checked with the candidate before a managed restart; protocol v1 does not claim hot binary replacement. See [Managed Xray Release Contract](docs/managed-xray-release-contract.md) for the capability matrix, activation rules, and rollout order.

Configuration validation commands are limited to 4 MiB, carry a monotonic version and SHA-256 digest, and are idempotent across Agent restarts. Older versions and a reused version with a different digest are rejected.

Existing node panels, Vector agents, and risk processing remain authoritative until their later migration gates are completed.

## Install

Create an enrollment token for an existing node in the central 3x-ui Nodes page. Run the generated installation command as root on that node. The command downloads a release archive, verifies its SHA-256, creates the dedicated `xui-agent` user, enrolls once, and starts the systemd service.

The equivalent command shape is:

```sh
curl -fsSL https://github.com/qqqasdwx/xui-agent/releases/latest/download/install.sh |
  env XUI_AGENT_ENROLLMENT_TOKEN='one-time-token' sh -s -- \
    --server-url 'https://panel.example.com/base-path'
```

Plain HTTP is rejected unless `--allow-insecure` is explicitly provided. This option is intended for isolated tests.

The installer preserves an existing configuration and identity. Supplying a new one-time token performs credential rotation without placing the token in a file. Re-running a newer release installer upgrades both the Agent binary and its root-owned runtime assets. It records the active binary as `previous`, switches `current` atomically, and waits for the new Agent's first authenticated heartbeat. A startup or health failure restores the previous binary and makes the installer fail explicitly.

Installations at `v0.3.3` or earlier must run the new Release installer once because those binaries only understand the retired signed-manifest schema. The parser accepts an existing `update.publicKey` value during this transition but ignores it; new installations do not write the field.

## Runtime Files

- `/etc/xui-agent/config.json`: non-secret runtime configuration, mode `0640`
- `/etc/xui-agent/runtime-assets.sha256`: root-owned digest of installed service and launcher assets, mode `0640`
- `/var/lib/xui-agent/identity.json`: node credential, mode `0600`
- `/var/lib/xui-agent/versions/`: Agent release binaries
- `/var/lib/xui-agent/xray-config/candidate.json`: latest shadow candidate, mode `0600`
- `/var/lib/xui-agent/xray-config/validation.json`: durable validation result, mode `0600`
- `/var/lib/xui-agent/xray-config/versions/`: immutable managed Xray configurations
- `/var/lib/xui-agent/xray-config/current.json`: active managed configuration symlink
- `/var/lib/xui-agent/xray-config/previous.json`: rollback configuration symlink
- `/var/lib/xui-agent/xray-config/applied.json`: confirmed managed configuration state
- `/var/lib/xui-agent/xray-runtime/versions/`: immutable Xray executable and geodata bundles
- `/var/lib/xui-agent/xray-runtime/current`: active managed Xray bundle symlink
- `/var/lib/xui-agent/xray-runtime/previous`: Xray binary rollback target
- `/var/lib/xui-agent/xray-runtime/applied.json`: confirmed managed Xray bundle state
- `/var/lib/xui-agent/current`: atomically selected Agent binary
- `/var/lib/xui-agent/previous`: rollback target
- `/usr/local/libexec/xui-agent-launcher`: stable systemd launcher

The launcher rolls back a pending release when the new binary exits before completing an authenticated heartbeat. A successful first heartbeat removes the pending marker and confirms the update.

Binary updates also compare the release manifest's runtime-assets digest with the root-owned marker written by the installer. If a release changes the launcher, systemd units, or uninstall script, the Agent rejects binary-only activation and reports that the release installer must run first. The unprivileged Agent never writes root-owned runtime assets.

Runtime assets in a release must remain compatible with the immediately previous supported Agent binary because a failed candidate is rolled back under the newly installed launcher and systemd units. A breaking runtime-asset change requires a staged compatibility release before the incompatible assets can ship.

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

Release tags build a deterministic Linux `amd64` archive. Manifest schema 3 binds the archive's size and SHA-256 plus its runtime-assets digest. The Agent constructs manifest and archive URLs itself from the fixed repository; the center can select only `latest` or a validated Release tag. The workflow requires no release-signing Secret and refuses to overwrite existing release assets. ARM nodes are not supported.

Local checks:

```sh
make verify
make release-snapshot
```

On a disposable systemd development VM with no existing xui-agent installation, the managed Xray lifecycle can be tested end to end with:

```sh
make integration-systemd
make integration-install-upgrade
```

Both integration targets refuse to run when the `xui-agent` account, units, configuration, state directory, or reserved test binaries already exist. They create and remove those resources during the test. `integration-install-upgrade` downloads the published `v0.2.0` archive, verifies a healthy installer upgrade to local candidates through a test control session, and then verifies automatic rollback after a startup failure.
