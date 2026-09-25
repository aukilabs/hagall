# Auki relay node

Standalone libp2p Circuit Relay v2 node. DDS provides authentication and peer
identity; DMS assigns relay bookings. This replaces the previous Hagall server.

## Run locally

Requires Go 1.23+, Make, access to private Auki Go modules, and running DDS/DMS
services with relay support.

```sh
export GOPRIVATE='github.com/aukilabs/*'
cp .env.example .env
```

Edit `.env` for your DDS/DMS endpoints and a local-test relay identity. Provision
these three files under `.local/` (or change the paths in `.env`):

- `registration.txt`: DDS-issued registration credentials, standard-base64 text
  encoding `<node UUID>:<registration secret>`.
- `wallet.key`: the relay wallet's secp256k1 private key in hex, optionally
  prefixed with `0x`. Keep the same wallet for an existing DDS registration.
- `libp2p.key`: a persistent Ed25519 key serialized with go-libp2p
  `crypto.MarshalPrivateKey`. Its Peer ID must match both advertised addresses.

Each file must be non-empty, at most 64 KiB, and readable only by its owner
(`chmod 600 .local/registration.txt .local/wallet.key .local/libp2p.key`).

Replace the hostname and `<relay-peer-id>` placeholders in
`RELAY_PUBLIC_BASE_MULTIADDRS`. Both TCP and WSS addresses are required. Use a
DNS hostname that resolves to the local relay; `localhost`, IP literals, and
`.local`/`.test` names are rejected as advertised addresses. WSS clients need a
TLS proxy forwarding the advertised WSS port to the relay's plaintext WS port
(`4002` in the example).

Load the configuration and start the relay from the repository root:

```sh
set -a
. ./.env
set +a
make run
```

The example binds all listeners to loopback. `RELAY_LOCAL_TEST_ALLOW_HTTP=true`
allows loopback HTTP for DDS/DMS; remote endpoints require HTTPS. Bookings start
disabled. After checking the advertised endpoints, set `RELAY_ACCEPT_BOOKINGS=true`
and restart to accept bookings. The live DMS recovery grace must exceed
`RELAY_SHUTDOWN_DRAIN` (the example uses `5s` for local runs).

Check startup with `curl --fail http://127.0.0.1:9090/readyz`; `/relay-info` shows
the redacted running configuration and `/accepting` shows booking eligibility.
Ctrl-C drains and relinquishes assignments before exit.

## Build and test

```sh
make build  # bin/auki-relay-node; override VERSION with a semantic version
make test  # formatting, vet, and tests
```

GitHub Actions runs these checks on pushes to `main`, `feature/*`, `bug/*`,
`chore/*`, and `hotfix/*`, and supports manual runs. It uses Go 1.23 and the
existing `GLOBAL_PUBLIC_GITHUB_APP_ID` / `GLOBAL_PUBLIC_GITHUB_APP_PRIVATE_KEY`
secrets to read the private `service-lib` module. The App needs Contents: read
access to that repository.

## Container images

Images are published to `docker.io/aukilabs/hagall` for Linux amd64 and arm64,
using the existing `DOCKER_USERNAME` and `DOCKER_PASSWORD` Actions secrets.

- Pushing a `vMAJOR.MINOR.PATCH` tag, optionally suffixed with an RC or date
  (for example `v1.0.0-RC-0` or `v1.0.0-20260908`), runs the checks above, then
  builds and pushes the image with that full version, the commit SHA, and `latest`.
- Publishing a GitHub release for that tag promotes the same image to `stable`,
  `vMAJOR`, and `vMAJOR.MINOR`. Releases marked as prereleases in GitHub are skipped;
  use that flag for RC releases to keep them out of `stable`. Promotion waits up
  to ten minutes for the versioned image; if its build fails, rerun promotion
  after fixing the tag build.
- Deployment steps are disabled: neither workflow calls Argo CD, SSH, or EC2.
  The dev relay image is pinned by digest in Terraform inputs.

To build the container locally after configuring private-module access:

```sh
go mod vendor
docker build --build-arg VERSION=v0.0.0 -t hagall:local .
```

The runtime contains the relay binary and CA certificates, and runs as UID/GID
`10001`. Credentials remain runtime configuration; local `.env` and identity
files are excluded from the build context.

## Kubernetes

The relay chart is published from [aukilabs/helm-charts](https://github.com/aukilabs/helm-charts/tree/main/charts/hagall)
at `https://charts.aukiverse.com`. It deploys one relay identity with an AWS NLB,
private admin/metrics Services, and optional Prometheus monitoring. The
[deployment wrapper](charts/hagall/README.md) pins its chart version and supplies
dev defaults. Argo CD receives environment settings and the image digest from
infrastructure inputs. Identity files come from an existing Kubernetes Secret.

Run `make chart-deps chart-check` to fetch the pinned chart, lint it, and render
the wrapper locally with fixtures.
Image-publishing workflows still do not trigger deployment.

## Capacity

Provider booking/reservation capacity is configuration, not a fixed 2048-slot
ceiling. `RELAY_LOCAL_CAPACITY` and the DDS registration capacity can be set to
values such as 800 or 10000 without rebuilding. The local default remains 32 and
the dev chart override remains 128. DMS's configurable provider ceiling defaults
to 2048; its separate organization quota remains an independent admission gate.
DMS may grant fewer slots than the signed capacity, but never more. Increasing
capacity does not assert that the hardware can sustain the resulting throughput.

Keep local total/IP/ASN reservation limits at least as large as the signed
capacity. Connection-manager low water must cover local capacity, high water
must exceed low water, and resource-manager connections/file descriptors/streams
must cover those watermarks. Admission-cache and memory budgets must also fit the
workload. Per-peer circuits retain a separate maximum of 256 (default 16).
The only fixed total-capacity bound is DMS's PostgreSQL INTEGER representation.
DMS response counts and byte budgets are bounded by the configured local capacity.

Deploy DMS migration `0013_relay_configurable_capacity` and the compatible
service/relay image before opting into larger chart values. Configuration changes
require a restart; active assignments remain fenced to their provider session.
The DMS database downgrade refuses persisted capacities above 256, including ended
session history; retain the newer schema while those records remain.

## Reservation recovery

When DMS rejects an assignment heartbeat with a conflict, the relay removes the
old fenced authority and schedules an authoritative Active reconciliation.
This discovers requester-triggered epoch rotations into `recovering`, which
ordinary booking claims do not return. The existing Recover/Ready exchange
restores the reservation; no new requester booking is required.

Recovery scans coalesce conflict bursts behind a five-second delay and retry
failed or ambiguous responses with bounded backoff. Healthy operation does not
poll Active. Recovery is independent of capacity and the new-booking gate, but
joins the same claim/drain barrier and starts no new pass once draining begins. The
five-second delay starts after conflict detection, not after the initial
disconnect; heartbeat timing and requester polling still affect total recovery.

## Source snapshot

Copied from `aukilabs/domain-service`, branch `feature/dds-p2p-demo`, commit
`8b2cda54c52285c666208057181e9f77b4c06f4c`.

`relay-node/cmd` and `relay-node/pkg` became `cmd` and `pkg`. Required helpers
were extracted from `pkg/authpkg` and `pkg/models/node.go`; imports now use
`github.com/aukilabs/hagall`. The original source is available at the recorded
commit; its standalone relay tree has since been removed from `domain-service`.

## Source authentication budgets

`RELAY_AUTH_MAX_ATTEMPTS_PER_IP` and `RELAY_AUTH_MAX_ATTEMPTS_PER_PEER`
limit attempts in `RELAY_AUTH_ATTEMPT_WINDOW`; they are independent of
`RELAY_AUTH_MAX_CONCURRENCY`, which bounds simultaneous authentication work.
For clients sharing an egress IP, a 512-attempt IP budget over 10 seconds can
coexist with 128 concurrent authentications and a 128-attempt per-peer budget.
The attempt cache and handshake lifetime remain independently bounded.

Private `auki_relay_node_source_auth_total` outcomes distinguish `rate_limited`
from `busy` (concurrency), `cache_full` (identity cardinality), and `context_done`
(shutdown/cancellation before verification). Older versions grouped all of these
under `rate_limited`. All still return the same token-free wire denial; clients
must not infer an authentication-versus-overload reason from that response.
