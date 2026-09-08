# Hagall relay chart

This chart runs a standalone relay behind an AWS Load Balancer Controller NLB.
Argo CD renders the chart from this repository; it does not need a chart registry.

Supply `relay.ddsUrl`, `relay.ddsPublicKeyUrl`, `relay.dmsUrl`, `relay.publicHost`,
`relay.peerId`, `identity.existingSecret`, `service.public.subnetIds`, and
`service.public.certificateArn` through infrastructure values. Use `image.digest`
to pin a published image. `image.tag` defaults to the chart's application version
when no digest is supplied.

The existing Secret must contain these file keys:

| Key | File content |
| --- | --- |
| `registration-credentials` | Base64 text encoding the DDS node UUID and secret, separated by `:` |
| `wallet-private-key` | Trimmed secp256k1 private-key hex |
| `libp2p-private-key` | Binary go-libp2p Ed25519 private key |

The init container copies the projected files into a memory-backed volume with
mode `0600`, owned by UID/GID `10001`. The runtime mounts that volume read-only.
Keep these same identities across restarts; do not generate keys in the chart.

Only zero or one replica is allowed. The Deployment uses `Recreate` to prevent
two pods from using the same identity. SIGTERM allows `relay.shutdownDrainSeconds`
for draining, with an additional 30 seconds in the pod termination grace period.

The public Service exposes raw TCP on port `443` and TLS-terminated WebSocket on
port `4443`, forwarded to relay ports `4001` and `4002`. The controller owns the
NLB and target groups. Keep Service names, selectors, ports, and load balancer
class stable when adopting an existing deployment. Admin port `9090` and metrics
port `9091` are ClusterIP-only; the optional PodMonitor scrapes the metrics port.

`values.dev.yaml` preserves the prototype's `auki-relay-node` resource names,
capacity of 128, admission settings, and enabled booking gate. Environment
endpoints, certificate, subnets, Peer ID, and image digest are supplied separately.

```sh
make chart-check
helm template hagall charts/hagall --namespace default \
  -f charts/hagall/values.dev.yaml -f /path/to/infrastructure-values.yaml
```

The `ci/values.yaml` file contains rendering fixtures, not deployment settings.
