FROM docker.io/oven/bun:1.3.0-alpine AS bun
FROM docker.io/library/node:22-alpine AS builder
WORKDIR /app
RUN apk add --no-cache python3 make g++ linux-headers
COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun
COPY package.json ./
RUN bun install
COPY . ./
ENV NEXT_TELEMETRY_DISABLED=1
RUN bun run build

FROM docker.io/library/node:22-alpine AS runner
WORKDIR /app
ARG SOURCE_REVISION
LABEL org.opencontainers.image.title="9router" \
      org.opencontainers.image.version="0.5.104-responses-fix" \
      org.opencontainers.image.revision="${SOURCE_REVISION}" \
      org.opencontainers.image.source="https://github.com/Artiffusion-Inc/9gouter"
ENV NODE_ENV=production PORT=20128 HOSTNAME=0.0.0.0 NEXT_TELEMETRY_DISABLED=1 DATA_DIR=/app/data
COPY --from=builder /app/public ./public
COPY --from=builder /app/.next/static ./.next/static
COPY --from=builder /app/.next/standalone ./
COPY --from=builder /app/custom-server.js ./custom-server.js
COPY --from=builder /app/open-sse ./open-sse
COPY --from=builder /app/src/mitm ./src/mitm
COPY --from=builder /app/node_modules/node-forge ./node_modules/node-forge
COPY --from=builder /app/node_modules/next ./node_modules/next
COPY --from=builder /app/node_modules/sql.js ./node_modules/sql.js
COPY --from=builder /app/node_modules/node-machine-id ./node_modules/node-machine-id
RUN mkdir -p /app/data /app/data-home && chown -R node:node /app && \
    ln -sf /app/data-home /root/.9router && apk add --no-cache su-exec && \
    printf '#!/bin/sh\nchown -R node:node /app/data /app/data-home 2>/dev/null\nexec su-exec node "$@"\n' > /entrypoint.sh && chmod +x /entrypoint.sh
EXPOSE 20128
ENTRYPOINT ["/entrypoint.sh"]
CMD ["node", "custom-server.js"]
