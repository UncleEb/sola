# syntax=docker/dockerfile:1

# ---- build: cross-compile a static binary on the native builder ----
# Pinned to the Go minor the module targets; --platform=$BUILDPLATFORM keeps the
# compiler running natively while emitting for the target arch (no emulation).
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src

# Module download as its own cache layer.
COPY go.mod go.sum ./
RUN go mod download

# Source. web/ and assets/ are compiled in via go:embed.
COPY . .

# buildx supplies TARGETOS/TARGETARCH per requested platform. CGO is off
# (modernc.org/sqlite is pure Go), so this cross-compiles with no C toolchain.
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/sola .

# A /data dir owned by the nonroot uid, so a fresh named volume inherits
# writable ownership (distroless has no shell to chown at runtime).
RUN mkdir -p /data

# ---- runtime: distroless static, nonroot ----
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/sola /sola
COPY --from=build --chown=65532:65532 /data /data

# config.json and the SQLite DB live in the mounted volume at /data.
ENV SOLA_CONFIG_DIR=/data
WORKDIR /data
EXPOSE 8088
VOLUME ["/data"]
USER 65532:65532

# distroless has no shell/curl, so the binary probes itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/sola", "healthcheck"]

ENTRYPOINT ["/sola"]
