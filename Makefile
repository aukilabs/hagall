VERSION ?= v0.0.0

.PHONY: build run test go-normalize go-vendor

build: go-vendor
	go build -mod=vendor -ldflags "-X main.version=$(VERSION)" -o bin/auki-relay-node ./cmd

# Load .env into the shell first; see README.md.
run: go-vendor
	go run -mod=vendor -ldflags "-X main.version=$(VERSION)" ./cmd

test: go-normalize
	go test -mod=vendor -p 1 ./...

go-normalize: go-vendor
	go fmt ./...
	gofmt -w scripts/patch-relay.go
	go vet -mod=vendor ./...

go-vendor:
	go mod tidy
	go mod vendor
	go run -mod=vendor ./scripts/patch-relay.go

.PHONY: chart-deps chart-check
chart-deps:
	helm repo add auki https://charts.aukiverse.com --force-update
	helm dependency build charts/hagall

chart-check:
	helm lint --strict charts/hagall -f charts/hagall/ci/values.yaml
	helm lint --strict charts/hagall -f charts/hagall/ci/values.yaml -f charts/hagall/values.dev.yaml
	helm template hagall charts/hagall --namespace default -f charts/hagall/ci/values.yaml >/dev/null
	helm template hagall charts/hagall --namespace default -f charts/hagall/ci/values.yaml -f charts/hagall/values.dev.yaml >/dev/null
