# Edge agent

The edge agent is the on-node control loop for a managed CDN edge ("被控节点"). It runs next to OpenResty, pulls desired configuration from the control plane over **outbound** mTLS, applies signed release artifacts atomically, reports health and node metrics, and executes lifecycle commands. The control plane never connects inbound: there is no SSH, no Docker socket, and no privileged container.

## Reconciliation loop

Each poll (`RunOnce`) is idempotent and last-known-good safe:

1. Load persisted state and re-send any pending ACK from a previous cycle.
2. `GET` the desired config with the cached `ETag`. `304 Not Modified` only refreshes the heartbeat.
3. If a lifecycle command is present, handle it (`drain`, `maintenance`, `rollback`) and ACK.
4. Otherwise, if the desired generation differs, download and apply the signed artifacts.
5. Report status + node metrics, then ACK the applied generation.

On any error the agent logs, keeps the current release, and retries with exponential backoff plus jitter. A control-plane outage never mutates the running release or local state.

## Signed artifact apply

`internal/apply` performs a crash-safe, atomic rollout:

- Verify every artifact's ed25519 signature against the enrolled trust bundle, check the manifest identity (`tenant/site/generation`), and enforce the size bound before extracting the deterministic tar into a staging release.
- Validate with `openresty -t` against the staging tree.
- Swap `current`/`previous` symlinks atomically, reload OpenResty, and probe the local health URL.
- **Auto-rollback**: any validation, reload, or health failure restores the previous release and reloads it.
- Old releases beyond `EDGE_RELEASE_RETENTION` are pruned; `current`/`previous` are always protected. A file lock serialises concurrent applies.

## Identity & enrollment

- **First boot**: the one-time bootstrap token is exchanged (CSR → signed client certificate). The identity and trust bundle are persisted under `<data_dir>/identity` and the host token file can then be deleted.
- **Steady state**: all control-plane requests use the persisted mTLS identity. The client certificate carries a single SPIFFE URI SAN: `spiffe://cdn-edge/<tenant-uuid>/<node-uuid>`.

## Node metrics

`internal/metrics` collects real node telemetry every report (best-effort: an unreadable source contributes `0` rather than failing the report):

- **CPU / memory / bandwidth** from `/proc` (`stat`, `meminfo`, `net/dev`); bandwidth reflects the data-plane interface and excludes loopback.
- **Disk** from `statfs` on the data volume.
- **Requests-per-second** from the loopback OpenResty `stub_status` endpoint (`EDGE_STATUS_URL`).
- **Error rate** from the active release's JSON access log (`EDGE_ACCESS_LOG_FILE`, defaulting to `<data_dir>/current/logs/access.log`).

Rate-based fields are deltas between polls, so the first report after start emits `0` for them until a second sample exists. Counter resets (reload/reboot) and log rotation are detected and re-baselined.

## Configuration

Configuration is loaded from an optional JSON file (`EDGE_AGENT_CONFIG_FILE`) and then overridden by environment variables.

| Variable | Default | Purpose |
| --- | --- | --- |
| `EDGE_CONTROL_PLANE_URL` | `https://127.0.0.1:3001` | Control-plane edge gateway (must be `https`). |
| `EDGE_CONTROL_PLANE_CA_FILE` | – | Absolute path to the gateway CA used to pin TLS. |
| `EDGE_BOOTSTRAP_TOKEN` / `_FILE` | – | One-time enrollment token (the entrypoint reads the file form). |
| `EDGE_DATA_DIR` | `/var/lib/cdn-edge-agent` | Persistent volume for identity, releases, and state. |
| `EDGE_POLL_INTERVAL` | `30s` | Base reconcile interval. |
| `EDGE_MAX_BACKOFF` | `5m` | Maximum backoff after failures. |
| `EDGE_JITTER_FRACTION` | `0.2` | Poll jitter (0–0.5) to de-synchronise a fleet. |
| `EDGE_COMMAND_TIMEOUT` | `30s` | Timeout for validate/reload commands. |
| `EDGE_ARTIFACT_MAX_BYTES` | `67108864` | Per-artifact download/extract bound. |
| `EDGE_RELEASE_RETENTION` | `5` | Number of old releases to keep. |
| `EDGE_VALIDATION_COMMAND` | `["openresty","-t","-p","{staging}"]` | Staging validation command (`{staging}` placeholder). |
| `EDGE_RELOAD_COMMAND` | `["openresty","-s","reload","-p","{current}"]` | Reload command (`{current}` placeholder). |
| `EDGE_HEALTH_URL` | `http://127.0.0.1:8080/healthz` | Loopback health probe after reload. |
| `EDGE_HEALTH_TIMEOUT` | `5s` | Health probe / status scrape timeout. |
| `EDGE_STATUS_URL` | derived from health host + `/nginx_status` | Loopback `stub_status` endpoint for RPS. |
| `EDGE_ACCESS_LOG_FILE` | `<data_dir>/current/logs/access.log` | JSON access log for error rate; `off` disables it. |

## Build & test

```sh
go build ./...
go test ./...
```

The container image is built as a static binary in the multi-stage runtime image at `deploy/edge/openresty/Dockerfile`; deployment is described in `deploy/edge/README.md`.

## Layout

```text
cmd/edge-agent      process entrypoint, config load, wiring
internal/agent      reconciliation loop, state, command handling
internal/apply      atomic release apply, validate, reload, rollback
internal/artifact   signed artifact verification and extraction
internal/config     configuration loading and validation
internal/control    control-plane client (bootstrap + mTLS)
internal/identity   persisted node identity and trust bundle
internal/metrics    node telemetry collection
```
