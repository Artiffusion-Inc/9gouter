# syntax=docker/dockerfile:1.7

# Multi-stage build producing a single static Go binary with the Next.js
# dashboard embedded via //go:embed. The runtime image is distroless: no
# shell, no package manager, no Node/Bun runtime — only the static binary and
# the embedded static-export UI (pre-built HTML/CSS/JS). Minimal footprint.
#
# Built for linux/amd64 + linux/arm64 via docker buildx --platform.

# ---------------------------------------------------------------------------
# Stage 1: Next.js static export (dashboard UI).
# The dashboard is UI-only after the Go rewrite. `bun run build` produces a
# fully static `out/` directory (output:export) which the Go binary embeds and
# serves. No JS runtime survives into the final image.
# ---------------------------------------------------------------------------
FROM docker.io/oven/bun:1.3-alpine AS dashboard
WORKDIR /build

ENV NEXT_TELEMETRY_DISABLED=1

# Deps first (layer cache).
COPY package.json bun.lock ./
RUN --mount=type=cache,target=/root/.bun/install/cache \
    bun install --frozen-lockfile --ignore-scripts

# Config + sources (build a static export, not a Node server).
COPY next.config.mjs postcss.config.mjs eslint.config.mjs jsconfig.json ./
COPY public ./public
COPY src ./src
COPY i18n ./i18n
# open-sse is imported by src/app via the jsconfig "open-sse" alias
# (providerModels, ttsModels, thinkingLevels, pricing, ...). It is legacy JS
# but the dashboard UI still reads these config modules at build time.
COPY open-sse ./open-sse

RUN bun run build

# ---------------------------------------------------------------------------
# Stage 2: Go binary (static, CGO=0) with the dashboard export embedded.
# TARGETARCH is injected by buildx per platform; we cross-build correctly for
# each architecture (no hardcoded GOARCH).
# ---------------------------------------------------------------------------
FROM docker.io/library/golang:1.26-alpine AS builder
WORKDIR /build

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
# Overlay the freshly-built static export onto the embedded-asset directory.
COPY --from=dashboard /build/out/ ./internal/adapter/transport/http/dashboard_assets/

ARG TARGETARCH=amd64
ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=${TARGETARCH}
ENV NEXT_TELEMETRY_DISABLED=1

# Static binary, stripped of symbols and DWARF for the smallest image.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags='-s -w -extldflags=-static' -trimpath \
    -o /out/9gouter ./cmd/9gouter

# ---------------------------------------------------------------------------
# Stage 3: distroless runtime. No shell, no Node/Bun, no package manager —
# only the static Go binary serving /v1, /api, and the embedded dashboard.
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

# Runtime configuration. Defaults match internal/adapter/config/config.go.
ENV DATA_DIR=/app/data
ENV PORT=20127
ENV HOSTNAME=0.0.0.0

COPY --from=builder --chown=nonroot:nonroot /out/9gouter /app/9gouter

USER nonroot:nonroot

EXPOSE 20127

# Persistent SQLite database + dashboard state lives here. Mount a volume.
VOLUME ["/app/data"]

ENTRYPOINT ["/app/9gouter"]