# xui-agent Project AGENTS.md

## Project Role

This repository contains the lightweight node agent for xui-stack. The agent runs under systemd, maintains an outbound authenticated connection to the central 3x-ui control plane, reconciles approved configuration, supervises the local Xray process, and delivers node events reliably.

The sibling repositories are:

- `../3x-ui/`: the central control plane and source of truth for Xray configuration
- `../Xray-core/`: the managed data-plane process and connection-time enforcement
- `../xui-riskd/`: the legacy collection and risk service being migrated into the control plane in stages

## Architecture Boundaries

- The central 3x-ui instance owns desired configuration. Do not introduce a second configuration source of truth on the node.
- The agent validates configuration before activation and preserves the last known good configuration for offline operation and rollback.
- The agent may supervise only explicitly supported Xray lifecycle operations. Do not expose arbitrary shell execution.
- Do not require a local 3x-ui panel, Docker daemon, Docker socket, or inbound public management API.
- Keep access events and routing events as separate ordered streams. Do not invent a one-to-one correlation contract between them.
- Risk evaluation, alerting policy, fleet views, and user administration belong in the central control plane, not in the agent.
- Connection-time enforcement belongs in Xray-core when accurate enforcement requires live protocol state.

## Security

- Use short-lived one-time enrollment tokens only for initial registration.
- Store a distinct revocable node credential after enrollment. Design credential rotation into the protocol.
- Authenticate and encrypt every control and event transport. Validate all messages at the boundary.
- Never log credentials, access tokens, subscription data, source IP data, or complete user traffic records at informational level.
- Verify signed update artifacts and provide automatic rollback after failed startup or health validation.
- Run under a dedicated system user with the minimum filesystem access and Linux capabilities needed to manage Xray.

## Protocol and Reliability

- Version cross-repository contracts explicitly under `protocol/`.
- Preserve backward compatibility during staged rollouts, or fail with a clear unsupported-version error.
- Fence duplicate sessions so only one active agent session controls a node identity.
- Persist outbound events before delivery. Use at-least-once delivery with stable event IDs and central idempotency.
- Acknowledge commands and event batches only after the corresponding local durable state transition succeeds.

## Engineering Conventions

- Use Go and standard-library facilities unless an external dependency removes substantial complexity.
- Keep packages focused and keep OS, network, process, and filesystem boundaries injectable for tests.
- Return explicit errors. Do not add mock success paths, silent fallbacks, or broad error swallowing.
- Treat agent self-update and Xray update as separate state machines with independent version and rollback state.
- Do not add production behavior without focused tests and an operational rollback path.

## Validation

Run the checks relevant to the change, including:

- `go test ./...`
- `go vet ./...`
- `go test -race ./...` for concurrency or lifecycle changes
- protocol compatibility tests for control-plane contract changes
- systemd and isolated Xray integration tests for lifecycle or update changes

Report checks that could not be run and why.
