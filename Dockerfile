# Mirage container image.
#
#   docker build --build-arg VERSION=v0.1.0 -t mirage .
#   docker run --rm -p 8484:8484 -v mirage-data:/data mirage
#
# The binary is static (CGO off) and runs as the unprivileged "nonroot" user of
# the distroless base image. State lives in the /data volume. Service
# listeners are opened inside the container, so publish their ports as well
# (for example -p 9001:9001) and point the services' upstreams at addresses
# the container can reach.

# ---- build ----
FROM golang:1.27-alpine AS build

ENV CGO_ENABLED=0
WORKDIR /src

# Download modules first so this layer is cached until go.mod or go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN go build -trimpath \
      -ldflags "-s -w -X github.com/justinburger/mirage/internal/version.Version=${VERSION}" \
      -o /out/mirage ./cmd/mirage \
 && mkdir -p /out/data

# ---- runtime ----
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
LABEL org.opencontainers.image.title="Mirage" \
      org.opencontainers.image.description="Service virtualization and traffic replay for performance testing" \
      org.opencontainers.image.source="https://github.com/justinburger/mirage" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=build /out/mirage /mirage
COPY LICENSE /LICENSE

# The data directory must belong to the nonroot user (uid 65532); a new named
# volume inherits this ownership.
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME ["/data"]

USER 65532:65532
EXPOSE 8484
ENTRYPOINT ["/mirage"]
CMD ["serve", "--admin", "0.0.0.0:8484", "--data", "/data/mirage.db"]
