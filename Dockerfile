# RouteGate production image (multi-stage, built by the Release Console).
# Stage 1: admin panel (React/Vite) -> internal/web/dist
# Stage 2: gateway binary with the embedded panel (-tags panel, CGO off)
# Stage 3: runtime (alpine + curl for the healthcheck, non-root user)
FROM node:22-alpine AS panel
WORKDIR /src/web/admin
COPY web/admin/package.json web/admin/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/admin/ ./
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Overwrite the source tree's (possibly empty) dist with the panel stage
# output: go:embed requires the real index.html at compile time.
COPY --from=panel /src/internal/web/dist /src/internal/web/dist
# Build-time version injection (ADR-0001 §15.3): /__metrics build_info and
# `routegate version` report this; the console/CI may pass ROUTEGATE_VERSION,
# local builds stay "dev".
ARG ROUTEGATE_VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -tags panel \
    -ldflags="-s -w -X main.version=${ROUTEGATE_VERSION}" -o /out/routegate ./cmd/routegate

FROM alpine:3.20
RUN apk add --no-cache curl tzdata && adduser -D -H -u 10001 routegate
COPY --from=build /out/routegate /usr/local/bin/routegate
COPY --from=panel /src/internal/web/dist /internal/web/dist
USER routegate
EXPOSE 8080 8081 8082
# The data plane (:8080) requires client certificates by design — probes and
# the compose healthcheck target the plaintext health listener on :8082.
HEALTHCHECK --interval=15s --timeout=3s --start-period=20s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8082/__ready || exit 1
ENTRYPOINT ["/usr/local/bin/routegate"]
CMD ["serve"]
