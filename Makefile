TAG ?= $(shell git rev-parse HEAD)
DOCKER_REGISTRY ?= aukilabs

# 
# Infra Build
# 
.PHONY : go-tidy go-vendor build

all: build

go-tidy:
	go mod tidy

go-vendor: go-tidy
	go mod vendor

build: go-vendor
	docker build -t $(DOCKER_REGISTRY)/hagall:${TAG} -t $(DOCKER_REGISTRY)/hagall:latest --build-arg VERSION=$(shell git describe --tags --abbrev=0) .

# 
# Dev Build
# 
install:
	@go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@go env -w GOPRIVATE=github.com/aukilabs/*
	@go mod tidy

run: go-build
	@./bin/hagall --hds.endpoint http://localhost:4002 --public-endpoint http://host.docker.internal:4000 --private-key-file data/test/privatekey.txt

go-build: go-normalize
	@mkdir -p bin
	@go build -ldflags "-X main.version=$(shell git describe --tags --abbrev=0)" -o bin/hagall ./cmd
	
go-normalize:
	@go fmt ./...
	@go vet ./...

help: go-build
	@./bin/hagall -h || true

test: go-normalize
	@go test -p 1 ./...

clean: services-stop go-tidy
	@-rm -rf bin
	@-rm -rf vendor

tag: check-version test
	@echo "\033[94m\n• Tagging ${VERSION}\033[00m"
	@git tag ${VERSION}
	@git push origin ${VERSION}

check-version:
	@echo "\033[94m\n• Checking Version\033[00m"
ifdef VERSION
	@echo "version set to $(VERSION)"
else
	@echo "\033[91mVERSION is not defined\033[00m"
	@echo "~> make VERSION=\033[90mv0.0.x\033[00m command"
	@exit 1
endif

bin/hagall:
	CGO_ENABLED=0 go build -mod vendor -ldflags "-X main.version=${VERSION}" -o ./bin/hagall ./cmd
