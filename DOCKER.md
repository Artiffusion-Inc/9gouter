# 9Gouter Docker / Podman Guide

## Pull the image

```bash
podman pull ghcr.io/artiffusion-inc/9gouter:latest
```

## Run

```bash
podman run -d \
  --name 9gouter \
  -p 20127:20127 \
  -v 9gouter-data:/app/data \
  --env-file .env \
  ghcr.io/artiffusion-inc/9gouter:latest
```

Dashboard: http://localhost:20127/dashboard
API: http://localhost:20127/v1

## Docker Compose

```bash
cp .env.example .env
docker compose up -d
```

The image is multi-arch (amd64 + arm64), distroless (no shell, no package manager).
Only the static Go binary + embedded Next.js static export survive in the image.
