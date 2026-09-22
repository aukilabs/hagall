# Staging relay: preflight, then activation

This is a **new, separate** Argo CD Application and Helm release named
`auki-relay-node`, in the staging cluster's `default` namespace. Keep the legacy
`hagall` Application, Deployment, Service, selectors, DNS and identity unchanged.
Do not enable the existing `hagall` wrapper switch to deploy this node.

Use chart path `charts/hagall`, published dependency `hagall` **1.0.0**, and pin
Argo `targetRevision` to the exact reviewed commit containing these files (or
reviewed PR head while testing). The historical `chart-v1.0.0` ref is not an
available rollout input; do not create it as part of this rollout.

## Infrastructure value contract

All keys are nested under **`hagall:`**. Preflight layering is the chart's
`values.yaml` → `values.staging.yaml` → infrastructure values. Never include
`values.dev.yaml` or `ci/values.yaml`. Argo Helm parameters override all these
files: audit them for accidental image, naming, replica or booking overrides.

Infrastructure supplies the following real staging values; none is provided by
this overlay and rendering without them intentionally fails schema validation:

| Key below `hagall` | Required value |
| --- | --- |
| `identity.existingSecret` | Name of operator-provisioned Secret in `default`; `auki-relay-node-identity` is an acceptable reference, not a created Secret |
| `relay.peerId` | Public Peer ID derived from that Secret's persisted libp2p key |
| `relay.publicHost` | Dedicated relay DNS hostname, not legacy Hagall's hostname |
| `relay.ddsUrl` | Staging DDS HTTPS base URL |
| `relay.ddsPublicKeyUrl` | Full staging DDS verification-key HTTPS URL |
| `relay.dmsUrl` | Staging DMS HTTPS API base including its required path prefix |
| `service.public.certificateArn` | Issued staging ACM certificate covering publicHost, in the NLB's region |
| `service.public.subnetIds` | YAML list of real public subnet IDs in the staging VPC |

The Secret must contain `registration-credentials`, `wallet-private-key`, and
`libp2p-private-key`. The chart references it and copies keys to memory-backed
0600 files; it never creates credentials or generates an identity. Verify the
public Peer ID out of band without printing private key or credential contents.

The overlay pins `aukilabs/hagall` release `v1.0.0-RC-0` by digest
`sha256:72550a08119d48e39b3d34d4e1a815624871a090a0b8a24b8f88ba2bcb00ba24`.
The rendered init and application images use `repository@digest`, not the tag.
One replica uses the dependency's `Recreate` strategy. Capacity and all local
reservation backstops are 128; DDS must sign that requested capacity and DMS must
agree. This is not a claim of load-tested capacity; values above 256 are invalid.

Resources are `auki-relay-node` (Deployment), `auki-relay-node-public` (NLB
Service), and `auki-relay-node-admin` (ClusterIP Service), with selector
`app.kubernetes.io/name: auki-relay-node`. Public ports are native libp2p TCP
443 → container 4001 (not HTTPS), and TLS-terminated WSS 4443 → container 4002.
Only WSS terminates TLS. Admin 9090 and metrics 9091 are ClusterIP-only; no
Ingress or PodMonitor is created. ClusterIP is not an authorization boundary:
restrict cluster access using existing network/RBAC controls. Never expose
admin through the public NLB. Add monitoring separately only with its CRD ready.

## Render and preflight

Local checks require Helm 3, Python 3 + PyYAML 6.0.2, and Git history containing
`f51fbf835936275b95c731ab7c7d70e68f21053b`. The existing CI workflow is unchanged;
run the additional staging target explicitly in a full-history local checkout.

```sh
make chart-deps chart-check chart-check-staging
helm template auki-relay-node charts/hagall --namespace default \
  -f charts/hagall/values.staging.yaml -f /path/to/infrastructure-values.yaml
```

1. Review the rendered resource names/selectors and infrastructure plan. Confirm
   legacy Hagall has no changes and no production/dev Application selects these
   staging overlays. Confirm image digest, identity, Peer ID, DNS, ACM and subnets.
2. With separately authorized deployment, sync only the new staging Application
   with `hagall.relay.acceptBookings: false`. This is a booking gate, **not** an
   offline mode: startup may register/authenticate with DDS/DMS and advertise
   endpoints. Complete identity/service registration approvals before sync.
3. Through private access, check `/livez`, `/readyz`, `/relay-info`, `/accepting`
   and metrics. Healthy readiness is independent of scheduling. Expect
   `accepting_bookings: false` and `bootstrap_closed` once ready, stable Peer ID,
   correct public multiaddrs, fresh control state and capacity agreement.
4. Verify DNS/NLB health, TCP 443 libp2p reachability and WSS 4443 certificate and
   protocol reachability. Verify 9090/9091 are not publicly exposed. A TCP socket
   or TLS handshake alone does not establish successful authenticated relaying.
   Keep bookings closed if any private/public check fails.

## Explicit activation and rollback

Only after recorded preflight evidence and operator approval, append
`values.staging-active.yaml` **last**, or set the equivalent explicit boolean
Helm value `hagall.relay.acceptBookings: true` in infrastructure. Use one source
of truth; do not retain a contradictory Helm parameter. This gate is static for
a process incarnation: changing it causes a `Recreate` rollout and brief outage.
The active overlay changes only the booking env var, not image/identity/network.

After activation, require `/accepting` to report accepting with fresh DMS state,
then exercise an authorized small robot booking, relay authentication, native
and WSS circuit/data transfer, completion and slot release. Negative auth and
unbooked reservation checks must remain deny-by-default. Do not call staging
production-ready based on manifest tests or preflight transport checks alone.

To stop new admission, remove the active overlay **and** any true Helm parameter,
render `RELAY_ACCEPT_BOOKINGS=false`, then perform an authorized rollout. Account
for existing circuits and the chart's 120-second drain / 150-second termination
grace. For an emergency private drain use the established operator procedure;
never publish `/drain`. Preserve the Secret and Peer ID through every rollback.
Do not delete, migrate or roll back the legacy Hagall Application as a substitute.

These files do not deploy anything. No dev/prod values, shared chart templates,
chart dependency version, runtime state, image tags or secrets are changed.
