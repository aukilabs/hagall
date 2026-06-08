# Suckless Agent Manual

## Principles
- Relay, historically Hagall, is a real-time WebSocket networking server for sessions, participants, entities, module fan-out, and in-memory state for late joiners. It is not a generic REST API service.
- Every Relay server has a unique Ethereum-compatible wallet. Never read, print, paste, or commit private keys, Kubernetes secrets, workflow tokens, or decrypted secret material.
- HDS, NCS, and the events endpoint are external runtime dependencies. Endpoint, auth, registration, health, smoke-test, receipt, or event changes can affect deployed dev/staging/prod behavior.
- The service is latency- and concurrency-sensitive. Treat goroutine lifetime, context cancellation, buffered channels, session cleanup, frame dispatch, and WebSocket compatibility as correctness concerns.
- Vendored dependencies are committed. Run `make go-vendor` after `go.mod` or `go.sum` changes.

## Where Things Live
- `cmd/main.go` — service entrypoint, config/env parsing, HDS pairing/unpairing, events logging, public HTTP/WebSocket routing, admin server, module registration, receipt handler wiring, private-key loading.
- `websocket/` — realtime message handling for participant/session/entity/component/custom-message flows, WebSocket auth hooks, metrics, log wrapping, payload limits.
- `models/` — in-memory sessions, participants, entities, entity components, frame handlers, broadcast helpers, and session store/discovery behavior.
- `modules/` — feature modules registered into realtime sessions: `vikja`, `odal`, `dagaz`, and `rosrelay`.
- `receipt/` — receipt payload verification and forwarding to Network Credit Service.
- `featureflag/` — hidden behavior toggles such as disabling session-state, broadcast, component, custom-message, or ROS topic relay behavior.
- `smoketest/` — HDS-triggered smoke-test support.
- `docs/` — user/operator-facing Relay documentation. Keep docs aligned with `README.md`, runtime config, admin endpoints, metrics, and deployment behavior.
- `.github/workflows/` — PR tests/build and tag/release automation via `aukilabs/go-tooling` reusable workflows. PR CI enables coverage, integration tests, and a tunnel. Do not copy workflow secrets or token values into docs.
- Deployment/config source of truth is outside this repo in `terraform-live`, `terraform-modules`, and `argocd-applications` chart values. This repo builds the `aukilabs/hagall` image; current environment image tags are deployment config, not code defaults.

## Git Branch Naming
- If a card or issue specifies a branch name, use it exactly.
- Otherwise prefer `feature/*` for new behavior, `chore/*` for maintenance/docs/deps/refactors, and `hotfix/*` for urgent production fixes.

## Workflow
1. Identify whether the change touches realtime WebSocket behavior, HDS registration/discovery, NCS receipts, events/logging, admin endpoints, docs, or build/deploy plumbing.
2. Read the nearby source and docs before editing. For runtime behavior start with `cmd/main.go`, then the relevant package under `websocket/`, `models/`, `modules/`, `receipt/`, or `featureflag/`.
3. Keep runtime/cloud/deployment changes out of this repo unless the task explicitly authorizes them. Deployed chart/Terragrunt/ArgoCD values live outside this repo.
4. This is Go module `github.com/aukilabs/hagall` on Go `1.23.0` with toolchain `go1.23.4`. Run `make go-normalize` for Go changes.
5. Run `make test` for substantive behavior changes; it runs serial `go test -p 1 ./...` and includes normalization first.
6. After `go.mod`/`go.sum` changes, run `make go-vendor` and include the resulting vendor updates intentionally.
7. For docs-only changes, at minimum verify `git diff --check`, scope of changed files, and that no secrets or line-number prefixes were copied.

## Tactics
- HDS is responsible for server registration, auth-token verification, health/readiness, smoke-test result submission, and session discovery. Review registration/unpair behavior carefully.
- NCS receives verified receipt payloads. The receipt channel is buffered and forwarding happens asynchronously; avoid blocking, leaks, or dropped context without understanding the tradeoff.
- The events endpoint receives structured logs/events. Changing event queue, flush, batch, transport, or endpoint behavior can alter observability in deployed environments.
- Public traffic uses the main HTTP/WebSocket server. Admin-only health, metrics, and pprof live on the admin server; do not expose admin endpoints publicly.
- Session state is in memory. Participant leave, entity delete, module state, frame ticker shutdown, HDS removal, and late-joiner state all need cleanup checks.
- Feature flags hide behavior changes. If a broadcast or ROS topic relay path changes, verify both enabled and disabled behavior where practical.
- Custom messages have an explicit size limit. Do not raise payload limits casually; consider latency, memory, and WebSocket compatibility.
- Integration tests need external HDS/tunnel-style dependencies and a temporary generated wallet. Do not use real wallets or existing-asset keys for tests.

## Code Review Focus
- Concurrency: goroutines have clear lifetimes, channels are sized/closed intentionally, request contexts are not used for background work that must outlive a connection.
- Realtime correctness: participant/session transitions preserve ordering, broadcast scope is correct, late joiners receive the expected state, and disconnects clean up resources.
- HDS/NCS/events: auth, registration, readiness, smoke results, receipt verification, and event delivery errors are surfaced without leaking sensitive payloads.
- Compatibility: protobuf/message changes remain compatible with existing clients and `hagall-common` expectations.
- Operations: admin endpoints stay private, readiness means HDS registration is healthy, metrics labels do not explode cardinality, and pprof is not made public.

## Communication
- Be terse, precise, and honest about uncertainty.
- State what changed, why, which runtime surface it affects, and what was verified.
- Flag risky changes involving auth, receipts/credits, HDS registration/discovery, public endpoints, admin exposure, private-key handling, or deployment configuration.

## Default Exit Checklist
- [ ] Only intended files changed.
- [ ] `git diff --check` passes.
- [ ] `make go-normalize` passes for Go changes.
- [ ] `make test` passes, or any skipped tests are explicitly justified.
- [ ] `make go-vendor` was run after `go.mod`/`go.sum` changes.
- [ ] No private keys, tokens, decrypted secrets, kube secret contents, or workflow secret values were copied.
- [ ] Response explains the change, affected surfaces, verification, and remaining risk.
