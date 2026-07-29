# xui-agent

`xui-agent` is the lightweight node runtime for xui-stack. It connects outward to the central 3x-ui control plane, applies approved Xray configuration, supervises the local Xray process, and delivers node events without requiring a full panel on every node.

## Status

The repository is in bootstrap phase and is not production-ready. Existing node panels, Vector agents, and risk processing remain authoritative until each migration phase is explicitly validated and rolled out.

## Runtime Boundary

- Central 3x-ui remains the configuration source of truth and retains Xray configuration capabilities.
- Central 3x-ui does not run, supervise, or package an Xray process in the production controller role.
- Each node runs `xui-agent` under systemd and an Agent-managed Xray process.
- The Agent initiates authenticated connections to the center; nodes expose no public management API.
- Access and routing events remain independent streams and are delivered through a persistent local queue.
- Risk analysis, alerting, fleet management, and user administration remain central responsibilities.

The first migration phase is read-only: enrollment, authenticated session management, heartbeat, capability reporting, configuration digest reporting, and signed Agent updates. It does not change the running Xray configuration or take production execution ownership.
