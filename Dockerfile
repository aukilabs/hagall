##
## Build-time
##
FROM --platform=$BUILDPLATFORM golang:1.21 as build
RUN mkdir -p /app/bin
WORKDIR /app
ADD . /app
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION
RUN make GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" arm_version="${TARGETVARIANT}" VERSION="${VERSION}" bin/hagall

##
## Run-time
##
FROM alpine:3
RUN addgroup -S hagall && adduser -S hagall -G hagall
USER hagall
WORKDIR /app
COPY --from=build /app/bin/hagall ./
ENTRYPOINT ["./hagall"]
