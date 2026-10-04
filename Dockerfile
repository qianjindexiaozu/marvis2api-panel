# Build on the builder's native architecture; only the final image is multi-arch.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.appVersion=${VERSION}" \
    -o /out/marvis2api ./cmd/server

FROM alpine:3
LABEL org.opencontainers.image.source="https://github.com/qianjindexiaozu/marvis2api-panel" \
      org.opencontainers.image.licenses="MIT"
RUN adduser -D -u 10001 app && mkdir -p /app/data && chown -R app:app /app
WORKDIR /app
COPY --from=build /out/marvis2api /app/marvis2api
COPY docker/entrypoint.sh /app/entrypoint.sh
COPY config.example.json /app/config.example.json
COPY LICENSE /app/LICENSE
RUN chmod +x /app/entrypoint.sh
USER app
ENV MV2A_PREPARE_FILE=/run/secrets/marvis.json
EXPOSE 18620
# Liveness only: /healthz legitimately returns 503 before an account is added.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:18620/panel/ 2>/dev/null | grep -q marvis2api || exit 1
ENTRYPOINT ["/app/entrypoint.sh"]
