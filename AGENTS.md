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
- Keep local setup in README.md and .env.example. Deployment configuration and
  CI/release automation are outside the scope of this extraction.
