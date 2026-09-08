VERSION ?= v0.0.0

.PHONY: build run test go-normalize go-vendor

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/auki-relay-node ./cmd

# Load .env into the shell first; see README.md.
run:
	go run -ldflags "-X main.version=$(VERSION)" ./cmd

test: go-normalize
	go test -p 1 ./...

go-normalize:
	go fmt ./...
	go vet ./...

go-vendor:
	go mod tidy
	go mod vendor
