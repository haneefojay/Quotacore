# syntax=docker/dockerfile:1
#
# The reference image (IP-15, deployment.md section 3). Two stages: the builder never reaches the
# runtime image, and the runtime image is the static binary and nothing else.
#
# The runtime is `scratch`, not a distroless base. A distroless static image ships five Debian
# packages, and three of them carry GPL-2 or MPL-2.0 text that the licence gate refuses (NFR-C4).
# `scratch` ships no package at all, so there is nothing copyleft, nothing to patch and nothing for
# the SBOM to leave out. The cost is that it has no CA roots: a deployment that verifies TLS to a
# managed data store or Postgres mounts the host's bundle and sets SSL_CERT_FILE (deployment.md
# section 3). The zone database is embedded in the binary (NFR-OPS8).
#
# The builder is pinned by digest (NFR-S7); `scratch` has no digest to pin.

FROM golang:1.27.1-bookworm@sha256:8d48e12ec56735e9358640898b9d9b9fcca110612ed8a5567438c0a1baa24e66 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY . .
ARG VERSION=0.1.0-dev
ARG REVISION=
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.buildVersion=${VERSION} -X main.buildRevision=${REVISION}" \
      -o /out/quotacore ./cmd/quotacore \
 && mkdir -p /out/tmp

FROM scratch
ARG VERSION=0.1.0-dev
ARG REVISION=
LABEL org.opencontainers.image.title="quotacore" \
      org.opencontainers.image.description="Authoritative quota enforcement for usage-priced software" \
      org.opencontainers.image.source="https://github.com/quotacore/quotacore" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.base.name="scratch"
COPY --from=build /out/quotacore /usr/local/bin/quotacore
# /tmp exists for a run without Compose's tmpfs; the service writes no file (IP-13 item 8).
COPY --from=build --chmod=1777 /out/tmp /tmp
ENV PATH=/usr/local/bin
# A fixed, unprivileged uid with no entry in an /etc/passwd the image does not have (NFR-S6).
USER 65532:65532
EXPOSE 8080
# No CMD: no argument serves, and `docker compose exec quotacore quotacore init` runs the CLI.
ENTRYPOINT ["/usr/local/bin/quotacore"]
HEALTHCHECK --interval=5s --timeout=3s --start-period=30s --retries=6 \
  CMD ["/usr/local/bin/quotacore", "healthcheck"]
