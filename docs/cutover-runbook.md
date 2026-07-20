# 9gouter Go Backend Cutover Runbook

This runbook describes how to move production traffic from the legacy Node.js
backend (`legacy/js-backend`) to the Go rewrite on `main`.

## 1. Prerequisites / Pre-flight

All gates below must be green before the cutover. Record the results.

| Gate | Command / Evidence | Expected Result |
|------|--------------------|-----------------|
| Golden contracts | `go test ./tests/golden/ -count=1` | All snapshot entries pass |
| Race-free test suite | `go test -race ./...` | All tests pass, no data races |
| Static binary build | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o 9gouter ./cmd/9gouter` | Binary produced |
| Cross-compile | `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o 9gouter-arm64 ./cmd/9gouter` | Binary produced |
| Shadow-diff clean | Run `tools/shadowdiff` on mirrored traffic for the agreed sample | No status/header/SSE/usage mismatches for in-scope routes |
| Soak bounded RSS | Run the Go binary under representative load for the agreed soak window (recommended: >= 4 hours) and sample RSS from `/proc/<pid>/status` or `docker stats` | RSS stays < 1 GB and shows no monotonic growth |
| Container image | `docker build -t 9gouter:cutover .` | Image builds and starts locally |
| Secrets / registry | `DOCKERHUB_USERNAME` + `DOCKERHUB_TOKEN` set in GitHub Actions secrets; GHCR uses `GITHUB_TOKEN` | Docker Hub + GHCR push configured |

## 2. Go-Live Steps

1. Ensure the merge base is green:
   ```bash
   git checkout main
   git pull origin main
   go test -race ./...
   ```

2. Build and push the cutover image:
   ```bash
   git tag -a v0.6.0-cutover -m "cutover to Go backend"
   git push origin v0.6.0-cutover
   ```
   The `docker-publish.yml` workflow will run `go test -race ./...`, build the
   Go binary, then build and push multi-platform images to:
   - `ghcr.io/artiffusion/9gouter:v0.6.0-cutover`
   - `ghcr.io/artiffusion/9gouter:latest`
   - `artiffusion/9gouter:v0.6.0-cutover`
   - `artiffusion/9gouter:latest`

3. On the production host, pull and start the single-service container:
   ```bash
   docker compose down
   docker compose pull
   docker compose up -d
   ```

4. Smoke-test the deployed container:
   ```bash
   curl -s http://localhost:20127/health
   curl -s http://localhost:20127/v1/models -H "Authorization: Bearer $TEST_API_KEY"
   curl -s http://localhost:20127/api/health
   ```

5. If all smoke tests pass, route live traffic to port `20127`.

## 3. Rollback Procedure

If error rate, latency, RSS growth, or behavioral drift exceeds acceptable
limits, roll back immediately:

1. Stop the Go container:
   ```bash
   docker compose down
   ```

2. Deploy the legacy JS backend image built from `legacy/js-backend`:
   ```bash
   git fetch origin legacy/js-backend
   git checkout legacy/js-backend
   # Build or use the last published legacy image, e.g.:
   docker build -t 9gouter-legacy:latest .
   # Or use the tag published before the cutover:
   # docker pull ghcr.io/artiffusion/9gouter:v0.5.XX-legacy
   docker run -d \
     --name 9gouter-legacy \
     -p 20128:20128 \
     -v 9gouter-data:/app/data \
     --env-file .env \
     9gouter-legacy:latest
   ```

3. Verify legacy health:
   ```bash
   curl -s http://localhost:20128/health
   ```

4. Route traffic back to port `20128`.

5. Postmortem: capture the failure mode and re-run the full shadow-diff window
   before the next cutover attempt.

## 4. Post-Cutover Monitoring Window

Watch the following for at least the agreed soak period (recommended: 4-24
hours):

| Metric | Source | Alert Threshold |
|--------|--------|-----------------|
| Error rate | Application logs / reverse proxy access log | > 0.5 % 5m baseline |
| SSE stall/abort terminal events | Logs containing `stall` / `abort` / `error SSE` | Any unexpected spike vs. baseline |
| Resident set size (RSS) | `docker stats 9gouter` or `/proc/<pid>/status` | > 1 GB or monotonic growth |
| Proxy failure rate | Logs containing `proxy_fallback` / `proxy_health` | > 1 % of upstream requests |
| End-to-end latency | Reverse proxy / load balancer metrics | P95 > agreed SLO |
| Dashboard login/session | Manual / automated smoke test | No auth regressions |

## 5. Legacy Branch Deletion Criteria

The `legacy/js-backend` branch may be deleted only after all of the following
have been true for the agreed stabilization period (recommended: >= 7 days):

1. Go binary is the sole production backend.
2. `go test -race ./...` is green in CI on every `main` commit.
3. Shadow-diff is clean over the most recent representative traffic sample.
4. No rollback to `legacy/js-backend` was required during the stabilization
   period.
5. RSS remained bounded < 1 GB throughout the soak and monitoring windows.
6. A final backup/tag of `legacy/js-backend` exists (e.g. `legacy/js-backend-final`).

When approved, delete the branch:

```bash
git push origin --delete legacy/js-backend
```
