# Docker edge node

This stack runs OpenResty and the Go edge agent as separate, least-privilege containers with one persistent `edge_data` volume. The volume owns `releases/`, the atomic `current`/`previous` links, and the node identity.

`edge-init` is a one-shot bootstrap that creates a health-only release and fixes volume ownership. It is the only root container and receives only the temporary file-ownership capabilities. OpenResty and the agent run as UID 65534 with read-only root filesystems and all capabilities dropped.

The agent joins OpenResty's network and PID namespaces. This permits local health checks and a same-UID Nginx reload without a Docker socket, SSH, or a privileged container. It validates signed staging releases with `openresty -t`, atomically changes `current`, reloads, waits for health, and automatically restores `previous` on failure.

## Runtime image publication and pinning

Unattended installation requires the GHCR package `ghcr.io/<owner>/onecdn-edge-kc-runtime` to be changed to **Public manually** in the GitHub package settings. The workflow publishes the package with `packages: write`; workflow YAML does not change package visibility. Run the `Edge Runtime Image` workflow successfully from the default branch at least once before onboarding a node. It publishes a `linux/amd64` and `linux/arm64` manifest and records the immutable `image@sha256:...` reference in the job summary.

Verify the public contract without cached credentials:

```sh
docker logout ghcr.io 2>/dev/null || true
docker pull ghcr.io/<owner>/onecdn-edge-kc-runtime:latest
docker buildx imagetools inspect ghcr.io/<owner>/onecdn-edge-kc-runtime:latest
```

For production, copy the digest from the workflow summary into `.env`:

```dotenv
EDGE_RUNTIME_IMAGE=ghcr.io/<owner>/onecdn-edge-kc-runtime@sha256:<digest>
```

`denied` or `unauthorized` normally means the package is not Public, the image path is wrong, or stale credentials are being used. `manifest unknown` normally means the default-branch workflow has not published the requested tag, or the digest/tag does not belong to this image. Check the workflow run and package tags before retrying. A short-lived, revocable token with only `read:packages` may be used interactively to diagnose a private package, but never place a PAT in an installer, `.env`, shell history, or the repository.

## Enrollment

1. Copy `.env.example` to a protected `.env`.
2. Place the public gateway CA and a one-time bootstrap token as described in `secrets/README.md`.
3. Validate and start:

   ```sh
   docker compose --env-file .env config
   docker compose --env-file .env up -d
   docker compose ps
   ```

4. After the agent is healthy, delete the host bootstrap-token file. The persistent identity is used for all subsequent mTLS requests.

The control plane never connects to this host and has no SSH or Docker API access. Rollout, drain, maintenance, and rollback are desired-state commands pulled by the agent over outbound mTLS.

## Networking

The edge runtime container uses host networking so signed stream services can bind arbitrary TCP/UDP ports (including custom listener ports) without regenerating Compose port mappings. Port 8080 serves HTTP and health checks. For an HTTP/3 site, allow the selected HTTPS port on both TCP and UDP (normally `443/tcp` and `443/udp`) through the host firewall, security group, NAT, and load balancer; a TCP-only listener cannot serve QUIC. Restrict `EDGE_BIND_ADDRESS` with the host firewall. TLS may terminate at a trusted load balancer or in a signed edge release; never expose cache/status endpoints to the public Internet.

## Node telemetry

Each report includes real node metrics rather than placeholders. Because the agent shares OpenResty's network and PID namespaces, it reads:

- CPU, memory and network throughput from `/proc` (bandwidth reflects the data-plane interface).
- Disk utilisation from `statfs` on the persistent volume.
- Requests-per-second from the loopback `stub_status` endpoint (`EDGE_STATUS_URL`, defaulting to `/nginx_status`), which stays private to `127.0.0.1`.

Rate-based fields are deltas between polls, so the first report after startup reports `0` for them until a second sample is taken.

Error rate is derived from the active release's JSON access log, which OpenResty writes to `current/logs/access.log` on the shared `edge_data` volume via the signed edge config. The agent reads that log over the same volume, so the error rate is reported by default. Set `EDGE_ACCESS_LOG_FILE=off` to disable it, or point it at a custom absolute path. Before the first site release there is no JSON access log yet, so the error rate reports `0` until a signed config is applied.
