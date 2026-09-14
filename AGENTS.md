# Relay node

- This repo contains the standalone libp2p Circuit Relay v2 node. The source
  snapshot and extraction paths are recorded in README.md.
- `cmd/` owns startup and signal handling; `pkg/` owns relay transport, DDS/DMS
  clients, authentication, bookings, admission, metrics, and draining.
- Keep changes independent of the `domain-service` checkout. Preserve DDS/DMS
  API contracts, token validation, deny-by-default admission, and identity
  continuity.
- Private keys, registration credentials, and tokens must never be printed or
  committed. Local configuration and identity files are ignored.
- Run `make test` and `make build` after Go changes. Run `go test -race -p 1 ./...`
  for concurrency changes. Tests use local fixtures and need no live DDS/DMS.
- After dependency changes, run `make go-vendor`. Vendor output is generated
  locally and ignored; commit go.mod and go.sum.
- Keep local setup in README.md and .env.example. `.github/workflows/build.yml`
  tests branch/tag pushes and publishes Docker Hub images for version tags.
  `.github/workflows/release.yml` promotes existing images to stable release tags.
  Keep test/build commands aligned with the Makefile.
- Keep private modules and credentials out of public caches, build artifacts,
  and runtime image layers. Vendor dependencies before building the Dockerfile.
- Deployment is deliberately disabled in these workflows until the new relay
  deployment is prepared. Preserve existing deployment secrets for that work.
- `charts/hagall` is a wrapper for the chart published from `aukilabs/helm-charts`.
  Keep reusable templates/schema/defaults in that repository and overrides here
  under `hagall:`. Run `make chart-deps chart-check` after chart changes and
  regenerate `Chart.lock` when changing the dependency version. Keep the dev
  resource names/selectors and identity stable;
  a shared identity permits only one replica, using the Recreate strategy.
- Keep environment-specific hosts, Peer IDs, subnets, certificates, and identity
  values in infrastructure inputs. The chart only references an existing Secret.
