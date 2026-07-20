#!/bin/sh
# Build and run 9gouter locally in a container (podman).
# Image build uses the Containerfile (distroless multi-stage build).
set -e

podman stop 9gouter 2>/dev/null || true
podman rm 9gouter 2>/dev/null || true
podman build -t 9gouter -f Containerfile .
podman run -d --name 9gouter -p 20128:20128 --env-file .env -v 9gouter-data:/app/data 9gouter