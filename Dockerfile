# syntax=docker/dockerfile:1.7

# BUILDPLATFORM/TARGETOS/TARGETARCH are populated automatically by buildx.
# Defaults make the file work with the legacy non-buildx builder too —
# locally `docker build` will produce a native-arch image without needing
# QEMU or buildx setup.
ARG BUILDPLATFORM=linux/amd64
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# ---- build stage ---------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26 AS build

WORKDIR /src

# Cache modules separately from the source so source-only changes don't
# re-download dependencies.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Re-declare inside the stage so the ARG values are visible to the build
# command. (FROM-line ARGs aren't automatically inherited by stages.)
ARG TARGETOS
ARG TARGETARCH

# Static, stripped binary so the final image can be FROM scratch.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/camonitor .

# ---- runtime stage -------------------------------------------------------
FROM scratch

# CA bundle so tsnet's ACME client can verify api.letsencrypt.org when
# requesting Tailscale's auto-provisioned HTTPS cert. The Debian-based
# build image ships this file; copying it is cheaper than switching to
# a distroless base.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=build /out/camonitor /camonitor

EXPOSE 8080

# Mount your config at /etc/camonitor/config.json (or override -config).
ENTRYPOINT ["/camonitor"]
CMD ["-config", "/etc/camonitor/config.json"]
