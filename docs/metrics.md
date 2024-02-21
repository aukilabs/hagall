# Metrics

Other than the standard Golang metrics coming from [prometheus/client_golang](https://github.com/prometheus/client_golang) with `go_*`, `process_*` prefixes etc., these additional metrics are available:

- `ws_*` - WebSocket related metrics
  - A very useful metric to look at is `ws_connected_clients`, which is a gauge that represents the number of connected WebSocket clients. Note that the smoke tests are also WebSocket clients, so it will flip between 0 and 1 during these tests.
