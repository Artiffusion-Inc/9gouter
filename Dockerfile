# syntax=docker/dockerfile:1.7

# === Build stage: install deps + build Next.js ===
FROM docker.io/oven/bun:1.3-alpine AS builder
WORKDIR /app

# Install dependencies (cached layer — bun.lock is the lockfile)
# --ignore-scripts skips native addon builds (better-sqlite3 unused on Bun runtime)
COPY package.json bun.lock ./
RUN --mount=type=cache,target=/root/.bun/install/cache \
    bun install --frozen-lockfile --ignore-scripts

# Build Next.js standalone output
COPY . .
ENV NEXT_TELEMETRY_DISABLED=1
RUN bun run build

# Prune dev deps for runtime
RUN --mount=type=cache,target=/root/.bun/install/cache \
    bun install --frozen-lockfile --production --ignore-scripts

# === Runtime stage: minimal Bun + Alpine ===
FROM docker.io/oven/bun:1.3-alpine AS runtime
WORKDIR /app

LABEL org.opencontainers.image.title="9router"

ENV NODE_ENV=production
ENV PORT=20128
ENV HOSTNAME=0.0.0.0
ENV NEXT_TELEMETRY_DISABLED=1
ENV DATA_DIR=/app/data

# Security: non-root user, su-exec for entrypoint
RUN apk --no-cache add su-exec && \
    adduser -D -u 1000 appuser && \
    mkdir -p /app/data /app/data-home && \
    chown -R appuser:appuser /app/data /app/data-home && \
    ln -sf /app/data-home /root/.9router 2>/dev/null || true

# Standalone Next.js output
COPY --from=builder /app/public ./public
COPY --from=builder /app/.next/static ./.next/static
COPY --from=builder /app/.next/standalone ./
# MITM child process and its deps
COPY --from=builder /app/src/mitm ./src/mitm
# Next file tracing can miss these; copy explicitly
COPY --from=builder /app/node_modules/node-forge ./node_modules/node-forge
COPY --from=builder /app/node_modules/next ./node_modules/next

# Fix ownership + entrypoint for runtime volume permissions
RUN chown -R appuser:appuser /app && \
    printf '#!/bin/sh\nchown -R appuser:appuser /app/data /app/data-home 2>/dev/null\nexec su-exec appuser "$@"\n' > /entrypoint.sh && \
    chmod +x /entrypoint.sh

EXPOSE 20128

ENTRYPOINT ["/entrypoint.sh"]
CMD ["bun", "run", "server.js"]