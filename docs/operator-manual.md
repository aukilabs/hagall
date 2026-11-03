# Run a relay node

The new Hagall node carries libp2p relay traffic between peers.
The Domain Discovery Service (DDS) authenticates the node.
The Domain Manager Service (DMS) assigns relay bookings.
A booking reserves relay capacity for a client.

This guide covers the new relay only.
Use the [Hagall README](https://github.com/aukilabs/hagall/blob/main/README.md) for developer instructions.
This repository owns the operator guide and check script.
The documentation site imports this guide.

## Before you start

You need:

- A Linux host with Docker and persistent private storage.
- A compatible Linux amd64 or arm64 image.
- A public IPv4 address or ingress that forwards both public relay ports.
- A DNS name with an IPv4 A record.
- A TLS proxy with a certificate valid for that name.
- Outbound HTTPS access to the authorized DDS and DMS services.
- Python 3.10 or later for the check script.
- Supported DDS enrollment and provisioned identity files.

Size CPU, memory, bandwidth, and file descriptors for the intended workload.
Configured capacity is not a measured throughput guarantee.
Do not apply old relay hardware requirements to this node.

### Obtain the identity files

Obtain these files through the supported provisioning process before installation:

| File | Required content |
| --- | --- |
| `registration.txt` | DDS-issued base64 text encoding `<node UUID>:<registration secret>`. |
| `wallet.key` | The registered wallet's secp256k1 private key in hexadecimal. An initial `0x` is optional. |
| `libp2p.key` | A persistent binary Ed25519 key serialized with go-libp2p `crypto.MarshalPrivateKey`. |

Obtain the Peer ID associated with `libp2p.key`.
Both advertised relay addresses must contain that Peer ID.
A wallet key and a libp2p key are different identities.
Do not substitute robot credentials for relay registration credentials.

**Stop if you lack a supported enrollment or key-provisioning route. Contact Auki before continuing.**
This guide does not supply a new enrollment service or key generator.
Check current registration, staking, and public-provider eligibility requirements with Auki.

Keep every file non-empty and no larger than 64 KiB.
Use owner-only permissions, such as `0600`.
The container runs as UID/GID `10001`.
Its account must be able to traverse the mounted directory and read the files.
Check ownership mappings before installation.
Do not change permissions on unrelated data.

Back up all three files securely.
Keep their values out of environment variables, images, logs, and public repositories.
Run one active process per identity.
Do not run multiple replicas with the same keys.

## Plan the public network

A **multiaddr** specifies a hostname, transport, port, and Peer ID.
Replace every placeholder below before use.
This example uses native TCP on public port `4001` and WSS on public port `443`:

```text
/dns4/relay.example.com/tcp/4001/p2p/<YOUR_PEER_ID>
/dns4/relay.example.com/tcp/443/wss/p2p/<YOUR_PEER_ID>
```

Both transports are required.
Use a lowercase public DNS name, not an IP literal or local-only hostname.
The advertised Peer IDs must match the persistent libp2p identity.

| Endpoint | Example public port | Destination | Exposure |
| --- | --- | --- | --- |
| Native libp2p TCP | `4001` | Relay TCP listener `4001` | Public |
| Secure WebSocket (WSS) | `443` | TLS proxy, then plaintext WS listener `4002` | Public TLS endpoint only |
| Admin | None | `127.0.0.1:9090` | Private |
| Metrics | None | `127.0.0.1:9091` | Private |

Native TCP uses libp2p/Noise, not HTTPS.
An ordinary HTTP reverse proxy cannot replace native TCP forwarding.
The TLS proxy must support WebSocket HTTP Upgrade.
It must forward WSS traffic to the private WS listener.
Never expose the plaintext WS listener, admin endpoint, or metrics endpoint publicly.
No UDP or QUIC port is required.
Public port `80` is necessary only when your certificate challenge requires it.

The [AWS chart](https://github.com/aukilabs/helm-charts/tree/main/charts/hagall) uses a different public mapping:

- Raw TCP `443` forwards to relay port `4001`.
- The TLS proxy for WSS `4443` forwards to relay port `4002`.

These are chart choices, not mandatory protocol ports.
Kubernetes is optional.

### Check the forwarding path

1. Point the DNS A record at the public IPv4 address.
2. Forward both public ports through the router, if applicable.
3. Permit those ports in the host firewall.
4. Permit those ports in applicable cloud security groups.
5. Configure certificate renewal for the TLS proxy.

Carrier-grade NAT usually prevents direct inbound access.
Obtain suitable public ingress if your provider cannot supply port forwarding.
A successful connection from inside the host network does not prove external access.

## Configure the node

Obtain authorized HTTPS URLs for DDS, its public signing key, and DMS.
The DMS URL includes `/v1`.
Do not copy local-test endpoints from the upstream [environment example](https://github.com/aukilabs/hagall/blob/main/.env.example).

Create `relay.env` with the following settings.
Replace the hostname, Peer ID, and service URL placeholders.

```text
RELAY_DDS_URL=<AUTHORIZED_HTTPS_DDS_URL>
RELAY_DDS_PUBLIC_KEY_URL=<AUTHORIZED_HTTPS_DDS_PUBLIC_KEY_URL>
RELAY_DMS_URL=<AUTHORIZED_HTTPS_DMS_V1_URL>
RELAY_REGISTRATION_CREDENTIALS_FILE=/run/relay/registration.txt
RELAY_WALLET_PRIVATE_KEY_FILE=/run/relay/wallet.key
RELAY_LIBP2P_PRIVATE_KEY_FILE=/run/relay/libp2p.key
RELAY_PUBLIC_BASE_MULTIADDRS=/dns4/relay.example.com/tcp/4001/p2p/<YOUR_PEER_ID>,/dns4/relay.example.com/tcp/443/wss/p2p/<YOUR_PEER_ID>
RELAY_TCP_LISTEN_MULTIADDR=/ip4/0.0.0.0/tcp/4001
RELAY_WS_LISTEN_MULTIADDR=/ip4/127.0.0.1/tcp/4002/ws
RELAY_ADMIN_ADDR=127.0.0.1:9090
RELAY_METRICS_ADDR=127.0.0.1:9091
RELAY_ACCEPT_BOOKINGS=false
RELAY_SHUTDOWN_DRAIN=60s
```

**Use this drain example only if the live DMS recovery grace exceeds 60 seconds.**
Check that value with the service operator before starting.
Otherwise, obtain compatible drain and container-stop settings.
Do not enable `RELAY_LOCAL_TEST_ALLOW_HTTP` for remote services.
Bookings remain disabled during the initial checks.

## Start the container

This example uses Linux host networking.
The relay and the host's TLS proxy share the same loopback interface.
A separate bridge-network proxy cannot reach another container's loopback listener.
Other layouts need private forwarding and corresponding listener settings.

1. Put the provisioned files in `relay-identity` under the current directory.
2. Check directory access and file ownership for container UID `10001`.
3. Configure the host TLS proxy to forward WSS to `127.0.0.1:4002`.
4. Select an operator-approved image version or digest.

The following image tag exists: `docker.io/aukilabs/hagall:v1.0.0-RC-6`.
It is a release candidate, not a claim of production approval.
Check compatibility with the selected DDS and DMS versions.
Do not assume a floating `latest` or `stable` image selects the intended version.

```sh
export RELAY_IMAGE=docker.io/aukilabs/hagall:v1.0.0-RC-6
docker pull "$RELAY_IMAGE"
docker run --name auki-relay --detach --restart unless-stopped \
  --network host --stop-timeout 90 --env-file relay.env \
  --mount "type=bind,source=$PWD/relay-identity,target=/run/relay,readonly" \
  "$RELAY_IMAGE"
```

The stop timeout allows the example drain plus additional shutdown time.
Do not add public admin or metrics mappings.
A running container alone does not prove successful registration or network access.

## Run the checks

Download the [read-only check script](../scripts/check_relay.py).
Save it as `check_relay.py`.
Review the file before executing it.
The script needs no Python packages.

It reads file metadata, not key contents.
It does not print response bodies or exception details.
It does not change firewall rules or create paid bookings.

### Check the host

Run this command on the relay host for the host-network example:

```sh
python3 check_relay.py local \
  --registration "$PWD/relay-identity/registration.txt" \
  --wallet "$PWD/relay-identity/wallet.key" \
  --libp2p "$PWD/relay-identity/libp2p.key" --uid 10001
```

Use an account that can inspect metadata within the identity directory.
Do not loosen key permissions to make a check pass.
Other network layouts require access to the relay's private network namespace.
The minimal relay container does not include Python.
Do not expose admin publicly to accommodate the script.

The local check inspects:

- Regular file type, size, owner, and permission bits.
- Local `/readyz` status.
- Redacted `/relay-info` state and advertised Peer ID consistency.
- A present, non-draining DMS session.
- The `/accepting` status.

The script does not check private-key contents or their cryptographic relationship.
Before activation, a closed booking gate reports `NOT TESTED`.

### Check external access

Run the public check from a separate machine outside the relay's network:

```sh
python3 check_relay.py public --host relay.example.com \
  --tcp-port 4001 --wss-port 443
```

Use your actual advertised hostname and ports.
The script checks all returned public IPv4 A records.
It tests native TCP connectivity and a certificate-verified WSS Upgrade.
WSS must return HTTP `101` and the correct `Sec-WebSocket-Accept` value.
An HTTP `200` response does not pass.
TLS verification remains enabled.

### Interpret the result

- `PASS`: the named preflight check passed.
- `FAIL`: the named check failed. Follow its corrective instruction.
- `NOT TESTED`: the script did not establish that condition.
- Exit `0`: the selected required preflight checks passed.
- Exit `1`: a required check failed.
- Exit `2`: the command arguments are invalid.

Neither mode proves an authenticated reservation, working circuit, or payment.
Both modes report these limits explicitly.
A successful TCP connection does not prove a libp2p/Noise handshake.
A local check does not prove public reachability.
The script cannot determine whether you actually used an external machine.

## Enable bookings

Save needed container logs before the replacement steps.
Keep `relay-identity` and `relay.env` during replacement.

1. Check that both local and external checks passed.
2. Set `RELAY_ACCEPT_BOOKINGS=true` in `relay.env`.
3. Stop the container with `docker stop auki-relay`.
4. Remove the stopped container with `docker rm auki-relay`.
5. Repeat the start command with the same identity files and approved image.
6. Repeat the local command with `--require-accepting`.

The old container must be removed before its name can identify a replacement.

With `--require-accepting`, a closed or unavailable booking gate fails the check.
A booked client must separately test a reservation and an actual transport round trip.
Use an authorized test client with an approved credit budget.
Do not treat the preflight as a substitute for that test.

## Maintain the node

Back up all three identity files before an update.
Disable new bookings before planned maintenance.
Use the deployment's drain procedure before stopping the process.
Allow the configured drain period.
Stop the old process before starting its replacement.

For the Docker example, `docker stop auki-relay` uses the configured stop timeout.
Retain the identity directory and `relay.env` during replacement.
Save any needed logs from the stopped container.
Remove only the stopped container with `docker rm auki-relay`.
Select the compatible image with `RELAY_IMAGE`.
Pull that image with `docker pull "$RELAY_IMAGE"`.
Repeat the start command with the same mounts.
Do not remove the identity directory or its files.
Restarting an existing container does not change its image.
Repeat the checks before accepting new bookings.

Do not replace a lost key silently.
Contact Auki for supported recovery if an identity file is lost.
A new Peer ID is not the former identity.

## Troubleshoot a failed check

- **File metadata:** check the path, size, UID, and private permissions.
- **Local readiness:** check startup logs and private admin connectivity.
- **DMS session:** check enrollment, outbound HTTPS, service availability, and drain state.
- **Peer ID:** compare both advertised addresses with the persistent identity.
- **DNS or TCP:** check the A record, firewall, router, and public forwarding.
- **WSS:** check certificate validity, hostname, TLS termination, and Upgrade forwarding.
- **Booking gate:** check the configured gate and DMS eligibility.

Inspect logs privately.
Remove credentials and private keys before sharing diagnostics.
A ready node can still reject bookings.
An idle node can be healthy without an active client.

## Understand rewards

Registration, readiness, bookings, traffic, earnings, and payment are different states.
A successful preflight establishes only the checks it names.
Rewards depend on eligible public ready slot-time under the applicable billing policy.
They do not follow from bytes transferred or process uptime alone.

Check current provider eligibility, revenue allocation, conversion, and payment policy with Auki.
Do not interpret an 80/20 allocation as a permanent policy or weekly payment guarantee.
Recorded credit earnings do not prove a finalized token payment.
An open PR does not establish that reward automation is deployed.
