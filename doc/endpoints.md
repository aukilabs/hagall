# Endpoints

* `/metrics` - Prometheus-formatted metrics
* `/health` - Health check endpoint, returns 200 OK if service is running
* `/debug/pprof/` - Index page of Go's [pprof](https://pkg.go.dev/net/http/pprof) package
  * There are many sub-endpoints useful for profiling. Please refer to the index page
