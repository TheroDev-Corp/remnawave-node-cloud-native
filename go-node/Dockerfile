# syntax=docker/dockerfile:1.7
# Go node image: a static Go binary plus xray-core, on scratch.
# Build from this directory:  docker buildx build -t remnawave-node-go go-node

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/node .


FROM alpine:3.21 AS xray
ARG XRAY_CORE_VERSION=v26.7.28
ARG UPSTREAM_REPO=XTLS
ARG TARGETARCH
RUN apk add --no-cache curl ca-certificates \
    && case "${TARGETARCH}" in \
        amd64) ARCHIVE='Xray-linux-64.zip' ;; \
        arm64) ARCHIVE='Xray-linux-arm64-v8a.zip' ;; \
    esac \
    && BASE="https://github.com/${UPSTREAM_REPO}/Xray-core/releases/download/${XRAY_CORE_VERSION}" \
    && curl -fsSL -o "/tmp/${ARCHIVE}" "${BASE}/${ARCHIVE}" \
    && curl -fsSL -o /tmp/xray.dgst "${BASE}/${ARCHIVE}.dgst" \
    && echo "$(awk '/^SHA2-256=/ { print $2 }' /tmp/xray.dgst)  /tmp/${ARCHIVE}" | sha256sum -c - \
    && unzip -q "/tmp/${ARCHIVE}" -d /tmp/xray \
    && mkdir -p /rootfs/usr/local/bin /rootfs/usr/local/share/xray /rootfs/var/log/xray /rootfs/tmp /rootfs/etc/ssl/certs \
    && install -m 755 /tmp/xray/xray /rootfs/usr/local/bin/xray \
    && ln -s /usr/local/bin/xray /rootfs/usr/local/bin/rw-core \
    && install -m 644 /tmp/xray/geoip.dat /tmp/xray/geosite.dat /rootfs/usr/local/share/xray/ \
    && cp /etc/ssl/certs/ca-certificates.crt /rootfs/etc/ssl/certs/ \
    && chmod 1777 /rootfs/tmp


FROM alpine:3.21 AS asn
ARG ASN_LMDB_URL=https://github.com/remnawave/asn-index/releases/latest/download/asn-prefixes.lmdb.zst
RUN apk add --no-cache zstd
ADD ${ASN_LMDB_URL} /tmp/asn-prefixes.lmdb.zst
RUN mkdir -p /out/asn \
    && zstd -d /tmp/asn-prefixes.lmdb.zst -o /out/asn/asn-prefixes.lmdb \
    && chmod 644 /out/asn/asn-prefixes.lmdb


FROM alpine:3.21 AS geocheck
ARG GEOCHECK_VERSION=0.3.0
ARG GEOCHECK_RELEASE_URL=https://github.com/remnawave/geocheck/releases/download
ARG TARGETARCH
RUN apk add --no-cache curl \
    && cd /tmp \
    && ARCHIVE="geocheck_linux_${TARGETARCH}.tar.gz" \
    && curl -fsSL -O "${GEOCHECK_RELEASE_URL}/v${GEOCHECK_VERSION}/${ARCHIVE}" \
    && curl -fsSL -O "${GEOCHECK_RELEASE_URL}/v${GEOCHECK_VERSION}/checksums.txt" \
    && grep "  ${ARCHIVE}\$" checksums.txt | sha256sum -c - \
    && tar -xzf "${ARCHIVE}" geocheck \
    && install -D -m 0755 geocheck /out/geocheck


FROM scratch

LABEL org.opencontainers.image.title="Remnawave Node (Go)"
LABEL org.opencontainers.image.description="Go port of the Remnawave node with xray-core"
LABEL org.opencontainers.image.source="https://github.com/TheroDev-Corp/remnawave-node-cloud-native"
LABEL org.opencontainers.image.licenses="AGPL-3.0-only"

COPY --from=xray /rootfs/ /
COPY --from=geocheck /out/geocheck /usr/local/bin/geocheck
COPY --from=asn /out/asn /usr/local/share/asn
COPY --from=build /out/node /usr/local/bin/remnawave-node


ENTRYPOINT ["/usr/local/bin/remnawave-node"]
