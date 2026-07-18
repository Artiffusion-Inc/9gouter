# syntax=docker/dockerfile:1.7

# Stage 1: build the Next.js dashboard static export.
# The dashboard is UI-only after the Go rewrite; it is embedded into the Go binary
# via //go:embed and served from internal/adapter/transport/http/dashboard_assets/.
FROM docker.io/oven/bun:1.3-alpine AS dashboard
WORKDIR /build

ENV NEXT_TELEMETRY_DISABLED=1

COPY package.json bun.lock ./
RUN --mount=type=cache,target=/root/.bun/install/cache \
    bun install --frozen-lockfile --ignore-scripts

COPY next.config.mjs postcss.config.mjs eslint.config.mjs jsconfig.json ./
COPY public ./public
COPY src ./src
COPY i18n ./i18n

RUN bun run build

# Stage 2: build the Go binary with the dashboard export embedded.
FROM docker.io/golang:1.26-alpine AS builder
WORKDIR /build

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=dashboard /build/out/ ./internal/adapter/transport/http/dashboard_assets/

ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64
ENV NEXT_TELEMETRY_DISABLED=1

RUN go build -ldflags='-s -w -extldflags=-static' -o /out/9router ./cmd/9router

# Stage 3: minimal distroless runtime image.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

# Default data directory. Mount a volume here for persistence.
ENV DATA_DIR=/app/data
ENV PORT=20127
ENV HOSTNAME=0.0.0.0

COPY --from=builder --chown=nonroot:nonroot /out/9router /app/9router

USER nonroot:nonroot

EXPOSE 20127

VOLUME ["/app/data"]

ENTRYPOINT ["/app/9router"]
