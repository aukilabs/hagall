FROM --platform=$BUILDPLATFORM golang:1.23 AS build
WORKDIR /app
COPY go.mod go.sum ./
COPY vendor/ ./vendor/
COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=v0.0.0
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -mod=vendor -trimpath -ldflags "-X main.version=${VERSION}" \
    -o /app/bin/auki-relay-node ./cmd

FROM alpine:3 AS runtime
RUN apk add --no-cache ca-certificates && \
    addgroup -S -g 10001 auki-relay-node && \
    adduser -S -u 10001 -G auki-relay-node auki-relay-node
USER 10001:10001
WORKDIR /app
COPY --from=build /app/bin/auki-relay-node ./auki-relay-node
EXPOSE 4001 4002 9090 9091
ENTRYPOINT ["./auki-relay-node"]
