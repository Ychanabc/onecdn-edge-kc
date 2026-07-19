# OneCDN Edge KC

Public data-plane components for running a OneCDN edge node. This repository intentionally contains only the edge agent, edge runtime, deployment examples, and release workflows. The control plane, deployment credentials, bootstrap tokens, and environment values are not included.

## Layout

- `services/edge-agent/` — Go reconciliation agent for outbound mTLS enrollment, signed release application, health, and telemetry.
- `deploy/edge/` — Docker Compose deployment and the OpenResty/ModSecurity runtime image.
- `.github/workflows/` — multi-architecture runtime and agent image publication plus tagged agent binaries.

## Quick start

```sh
cp deploy/edge/.env.example deploy/edge/.env
# Set the control-plane URL and create the local secret files described in
# deploy/edge/secrets/README.md. Never commit those files.
docker compose --env-file deploy/edge/.env -f deploy/edge/compose.yml config
docker compose --env-file deploy/edge/.env -f deploy/edge/compose.yml up -d
```

Build and test the agent:

```sh
cd services/edge-agent
go test ./...
```

Build images from the repository root:

```sh
docker build -f deploy/edge/openresty/Dockerfile -t onecdn-edge-kc-runtime:local .
docker build -f services/edge-agent/Dockerfile -t onecdn-edge-kc-agent:local .
```

Published images use:

- `ghcr.io/<owner>/onecdn-edge-kc-runtime`
- `ghcr.io/<owner>/onecdn-edge-kc-agent`

Use immutable digests in production. GitHub Actions records the digest in each workflow summary. Package visibility must be set to Public in GHCR package settings if anonymous pulls are required.

## Security

Treat `deploy/edge/.env`, everything under `deploy/edge/secrets/` except its README, node identity data, and bootstrap tokens as secrets. The included `.gitignore` excludes them, but operators remain responsible for secret storage and rotation.

## License

MIT; see [LICENSE](LICENSE).
