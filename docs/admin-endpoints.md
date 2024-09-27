# Admin Endpoints

Other than the ordinary port used for WebSocket traffic, the Relay server is also listening on a separate port for administrative purposes.
This port is not exposed externally by default and should not be, so you would have to connect to it from the same machine as you're running the Relay server on or from your internal network only.

```shell
    --admin-addr                string               Admin listening address.
                                                     Env:     HAGALL_ADMIN_ADDR
                                                     Default: ":18190"
```

For example, if the Relay server is running on your local machine and you visit http://localhost:18190/debug/pprof/ you can get profiling data.
If you set up Prometheus, you can scrape metrics from the /metrics endpoint.

| Endpoint | Description                                                                    |
| -------- | ------------------------------------------------------------------------------ |
| `/metrics`| Prometheus-formatted metrics                                                  |
| `/health` | Health check endpoint, returns 200 OK if service is running                   |
| `/debug/pprof/` | Index page of Go's [pprof](https://pkg.go.dev/net/http/pprof) package   |
