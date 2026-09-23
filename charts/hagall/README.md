# Hagall deployment wrapper

This chart pins `hagall` version **1.1.0** from `https://charts.aukiverse.com`.
Templates, defaults, schema, and operator instructions are maintained in
[aukilabs/helm-charts](https://github.com/aukilabs/helm-charts/tree/main/charts/hagall).
This repository supplies the wrapper and dev defaults used by Argo CD.

All overrides are nested under `hagall:`. `values.dev.yaml` preserves the
`auki-relay-node` resource names and selector, admission limits, and enabled
booking gate. Dev allows 2,048 reservations, with the existing 128-circuit
per-peer limit. The upstream chart sizes connection, stream, file-descriptor,
and admission-cache budgets from capacity. Deploy Hagall RC-1 and DMS v0.6.0
(including migration 0013), with DMS provider and organization ceilings of 2,048,
before using this configuration. Infrastructure supplies the existing identity Secret,
DDS/DMS endpoints, public host, Peer ID, image digest, certificate, and subnets.

```sh
make chart-deps
make chart-check
helm template hagall charts/hagall --namespace default \
  -f charts/hagall/values.dev.yaml -f /path/to/infrastructure-values.yaml
```

`make chart-deps` registers the Auki chart repository and builds the dependency
from the committed `Chart.lock`.
`ci/values.yaml` contains rendering fixtures, not runtime credentials. When
upgrading the dependency, update `Chart.yaml` and regenerate `Chart.lock` with
`helm dependency update charts/hagall` after the package has been published.

## First release of this wrapper

1. Merge and publish `hagall` 1.0.0 from helm-charts.
2. Run the wrapper CI checks and merge this wrapper in Hagall.
3. Tag that merged commit `chart-v1.0.0`.
4. Set `hagall_relay.chart_revision = "chart-v1.0.0"` and
   `hagall_relay.use_wrapper_chart = true` in Terraform live configuration.
5. Apply and sync the Hagall Application through the existing scoped procedure.

The Terraform wrapper option nests infrastructure overrides under `hagall:`.
Its default remains false for the earlier standalone chart. Keep the Helm
release name, namespace, resource names, image digest, identity Secret, and
network settings when switching. Chart-version labels change; the relay
configuration is preserved. Automatic deployment remains disabled.
