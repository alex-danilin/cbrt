# syntax=docker/dockerfile:1
#
# cbrt container image: a static, stdlib-only Go binary on `scratch`.
#
#   docker build -t cbrt .
#   docker run --rm -p 127.0.0.1:8080:8080 \
#     -v "$PWD/deploy/cbrt.docker.json:/etc/cbrt/cbrt.json:ro" \
#     -e CBRT_API_KEY=... -e NVIDIA_API_KEY=... -e OLLAMA_BASE_URL=http://host.docker.internal:11434/v1 \
#     -v cbrt-data:/data --read-only cbrt
#
# No secrets are baked in: config and keys come from mounts / environment at run time.
# There is no shell in the final image; the HEALTHCHECK uses the binary's own
# `cbrt healthcheck` subcommand (GET /healthz). Orchestrators can probe /healthz directly.

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# Only what the build needs. The dashboard HTML is embedded from internal/server/web via go:embed.
COPY go.mod ./
COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/cbrt ./cmd/cbrt

# Runtime scaffolding: a non-root user, an empty data dir owned by it, and a config dir.
RUN mkdir -p /out/data /out/etc/cbrt \
 && echo 'cbrt:x:65532:65532:cbrt:/data:/sbin/nologin' > /out/passwd \
 && echo 'cbrt:x:65532:' > /out/group

FROM scratch
# CA roots for HTTPS to cloud providers (e.g. integrate.api.nvidia.com).
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/passwd /etc/passwd
COPY --from=build /out/group /etc/group
COPY --from=build /out/cbrt /cbrt
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build /out/etc/cbrt /etc/cbrt

USER 65532:65532
WORKDIR /
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/cbrt", "healthcheck"]
ENTRYPOINT ["/cbrt"]
CMD ["serve", "-config", "/etc/cbrt/cbrt.json", "-env-file", "/etc/cbrt/.env"]
