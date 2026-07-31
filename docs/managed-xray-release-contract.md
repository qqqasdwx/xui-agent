# Managed Xray Release Contract

## Scope

This contract covers Xray installation and updates performed by `xui-agent` in `managed` mode. The central 3x-ui control plane owns the desired version. The Agent verifies, installs, activates, and rolls back the node-local runtime. Agent self-update and Xray update remain independent state machines.

## Compatibility Matrix

| Control protocol | Agent state | Advertised capabilities | Center behavior |
| --- | --- | --- | --- |
| v1 | `observe` | `observe`, optionally `config_validate` | Never sends configuration apply or Xray update commands. |
| v1 | `managed`, no managed bundle applied | `config_validate`, `config_apply`, `xray_update` | The configured Xray path is the bootstrap fallback for the first managed bundle. |
| v1 | `managed`, managed bundle applied | `config_validate`, `config_apply`, `xray_update` | Uses only the selected managed bundle; the bootstrap path is ignored. |
| v1 | older Agent without `xray_update` | no `xray_update` | Queued updates are rejected before dispatch with `unsupported_capability`; an already applied version becomes `unverified`, not falsely `drifted`. |

The center does not infer support from the Agent version string. A capability must be present both when an update is queued and immediately before it is dispatched.

The bootstrap path is not a parallel runtime source. It remains active only while no managed `current` selection and no managed `applied` marker exist. Selecting the first candidate immediately switches configuration validation, process supervision, service startup, and status collection to that candidate. If an `applied` marker exists but `current` is missing or invalid, the Agent reports corruption instead of silently falling back.

An Xray release is compatible only when all of these checks pass:

1. Manifest schema 2 is downloaded from the fixed `qqqasdwx/Xray-core` GitHub Release source selected by the requested tag or `latest`.
2. The manifest contains exactly one artifact for the node OS and architecture, and its size and SHA-256 match the downloaded archive.
3. The Agent constructs the archive URL from the resolved manifest version and fixed repository; the center does not provide a download URL.
4. The bundle contains `xray`, `geoip.dat`, and `geosite.dat`; the candidate reports the manifest's separate `xrayVersion` value.
5. If a managed configuration already exists, the candidate accepts that exact configuration with `xray run -test` using the candidate geodata.

This runtime validation is the Xray configuration compatibility gate. A version that cannot load the current node configuration is rejected before the active runtime is stopped.

## Release and Bundle

The maintained `qqqasdwx/Xray-core` release publishes:

- `xray-manifest.json`
- Linux ZIP archives for `amd64`, `arm64`, and `armv7`

The manifest `version` is the unique Fork Release tag, for example `v26.7.28-xui.1`. `xrayVersion` is the value reported by the executable, for example `26.7.28`. The manifest also binds publication time, platform, archive size, and SHA-256. GitHub is the release trust root; the Agent accepts neither a repository nor an artifact URL from the center.

The Agent installs the executable and both geodata files as one immutable bundle under `xray-runtime/versions`. It never combines an executable from one release with geodata from another release.

## Activation

Hot replacement is not part of protocol v1.

- With no applied configuration, the first bundle is installed and pinned without starting Xray. Normal configuration validation and apply commands start it later.
- With an applied configuration, the candidate is validated first. The Agent then stops the current process, switches the complete runtime bundle atomically, and starts a replacement process. On the first managed-bundle installation, the current process may still use the configured bootstrap executable.
- The replacement must become stable within the managed process health deadline. Failure restores the previous managed bundle, or the bootstrap executable when no managed bundle has yet been confirmed, and restarts it using a recovery context that does not inherit control-channel cancellation.
- A repeated command for the already applied version and archive is idempotent and does not restart Xray.

`current`, `previous`, the confirmed applied target, and both targets referenced by a pending update are protected from garbage collection. Additional recent valid versions are retained for diagnosis and rollback.

## Rollout and Rollback Order

Rollout order:

1. Deploy a center that understands the optional v1 fields and `xray_update` capability.
2. Publish checksummed Xray release manifests from the maintained fork.
3. Upgrade the Agent; an existing managed node keeps running its bootstrap Xray and advertises `xray_update` after the upgraded Agent reconnects.
4. Queue a pinned Release tag or `latest`. A successful `latest` result is persisted as the resolved Release tag.

Rollback order:

1. Stop issuing Xray update commands.
2. Request the previous Xray release. The same candidate validation and managed restart path applies to downgrades.
3. Roll back the Agent separately only when needed. An older Agent cannot receive Xray update commands, but it can keep the last applied Xray and configuration running.

The center never advances the applied Xray version on timeout, unsupported capability, validation failure, activation failure, or rollback failure.
