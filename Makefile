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

integration-tests:
	@pip3 install -q web3
	@python3 -c "from web3 import Web3; w3 = Web3(); acc = w3.eth.account.create(); print(f'{w3.to_hex(acc.key)}')" > hagall-private.key
	@chmod 400 hagall-private.key
	@HAGALL_PUBLIC_ENDPOINT="$$TUNNEL_URL" \
		HAGALL_PRIVATE_KEY_FILE="hagall-private.key" \
		HAGALL_HDS_ENDPOINT="https://hds.sandbox.aukiverse.com" \
		HAGALL_LOG_LEVEL=debug \
		HAGALL_HDS_REGISTRATION_INTERVAL=3s \
		go run ./cmd &
	@for i in $$(seq 1 5); do echo "Checking Hagall, attempt $$i"; curl --output /dev/null --verbose --fail http://localhost:4000/ready; code=$$?; test "$$code" = 0 && break; sleep 2; done; test "$$code" = 0 || (echo "Timeout when waiting for Hagall"; exit 1)
	@SCENARIO_NAME=integration-test \
		SCENARIO_HDS_ADDR="https://hds.sandbox.aukiverse.com" \
		SCENARIO_HAGALL_ADDR="$$TUNNEL_URL" \
		SCENARIO_LOG_LEVEL=debug \
		go run github.com/aukilabs/hagall-common/scenariorunner/cmd
