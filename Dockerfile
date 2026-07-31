# syntax=docker/dockerfile:1

# The build stage runs on the build host's architecture and cross-compiles for
# the target: compiling the vendored Postgres parser under QEMU emulation takes
# tens of minutes, a cross gcc takes the usual few. The release workflow builds
# on amd64, so arm64 is the only cross target this file knows how to reach.
#
# bookworm is deliberate: the binary links glibc through cgo, so the builder's
# glibc must not be newer than the runtime image's. Both stages are Debian 12.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

ARG TARGETARCH

# The cross toolchain is only needed when the target differs from the host.
RUN if [ "$TARGETARCH" != "$(dpkg --print-architecture)" ]; then \
        apt-get update \
        && apt-get install -y --no-install-recommends gcc-aarch64-linux-gnu \
        && rm -rf /var/lib/apt/lists/*; \
    fi

WORKDIR /src

# Modules before source, so editing code does not invalidate the download layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev

RUN set -eu; \
    if [ "$TARGETARCH" = "$(dpkg --print-architecture)" ]; then cc=gcc; \
    elif [ "$TARGETARCH" = "arm64" ]; then cc=aarch64-linux-gnu-gcc; \
    else echo "no cross compiler installed for $TARGETARCH" >&2; exit 1; fi; \
    CGO_ENABLED=1 GOOS=linux GOARCH="$TARGETARCH" CC="$cc" \
    go build -trimpath \
        -ldflags "-s -w -X github.com/tochemey/mig/internal/cli.Version=$VERSION" \
        -o /out/mig ./cmd/mig

# base rather than static: cgo links glibc dynamically. The image carries the
# CA bundle, so DSNs with sslmode=verify-full work out of the box.
FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=build /out/mig /usr/local/bin/mig

ENTRYPOINT ["/usr/local/bin/mig"]
