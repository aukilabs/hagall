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

## Source snapshot

Copied from `aukilabs/domain-service`, branch `feature/dds-p2p-demo`, commit
`8b2cda54c52285c666208057181e9f77b4c06f4c`.

`relay-node/cmd` and `relay-node/pkg` became `cmd` and `pkg`. Required helpers
were extracted from `pkg/authpkg` and `pkg/models/node.go`; imports now use
`github.com/aukilabs/hagall`. The original source remains in `domain-service`.
