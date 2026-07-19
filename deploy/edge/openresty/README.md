# HTTP/3-capable Nginx edge runtime

This directory is the baseline runtime for signed releases produced by `the signed release compiler`. It builds upstream Nginx with the HTTP/3/QUIC module, OpenResty-compatible LuaJIT/lua-nginx-module, ModSecurity v3, OWASP Core Rule Set, and the Go edge agent. It is deliberately not the stock OpenResty image: stock OpenResty does not currently provide HTTP/3/QUIC.

## Traffic path

- Port `8080` is the internal HTTP listener and exposes `/healthz`.
- HTTP/3-enabled signed site releases listen on the configured TLS port over both TCP (HTTP/1.1/HTTP/2) and UDP (QUIC/HTTP/3), require TLS 1.3 for QUIC, and advertise `Alt-Svc`. The compatibility template uses `8443`; production defaults to `443` when no site port is configured. Open UDP as well as TCP in the host firewall/load balancer.
- Site releases configure validated upstreams, proxy caching, request IDs, fixed security headers, and ModSecurity/CRS plus per-site custom rules.
- `/nginx_status` is restricted to loopback for the official Nginx Prometheus exporter. Never expose it directly.

The control-plane mTLS gateway is a separate trusted ingress. It must remove all client-supplied `X-Edge-*` headers, validate the node certificate URI SAN, and generate the API HMAC assertion. This runtime does not synthesize those identity headers.

## Build

```sh
docker build -f deploy/edge/openresty/Dockerfile -t cdn-edge-runtime:local .
```

The Dockerfile pins Nginx, LuaJIT/lua-nginx-module, ModSecurity connector/library, and CRS versions as build arguments and adds the statically built Go edge agent. It compiles `--with-http_v3_module`, `--with-stream`, Lua and ModSecurity into one binary; update and test those pins together because the Nginx module ABI is sensitive.

## Verification

```sh
docker build -f deploy/edge/openresty/Dockerfile -t cdn-edge-runtime:local .
docker run --rm --entrypoint /usr/local/openresty/bin/openresty cdn-edge-runtime:local -V 2>&1 | grep -- --with-http_v3_module
docker run --rm --entrypoint /usr/local/openresty/bin/openresty cdn-edge-runtime:local -t
```

After a TLS HTTP/3 release is active and TCP/UDP are reachable, verify end-to-end with:

```sh
curl --http3-only -I https://edge.example.com/
```

## Runtime mounts

Mount writable cache and log directories and, when enabling the TLS template, mount certificate files read-only:

```text
/var/cache/openresty
/var/log/openresty
/run/secrets/edge_tls_certificate.pem
/run/secrets/edge_tls_private_key.pem
```

`trusted-proxies.conf` trusts only loopback by default. Replace it with the smallest explicit load-balancer CIDR set; do not use `0.0.0.0/0` or `::/0`.

The templates are compiler inputs, not text-substitution inputs. Hostnames, upstreams, paths, and headers must pass the deterministic compiler validation before appearing in a release.

## Observability

- `vector/vector.yaml` parses OpenResty and ModSecurity JSON logs, writes structured output, and exports Vector internal metrics on port `9598`.
- Run `nginx/nginx-prometheus-exporter` as a host service or sidecar using `prometheus/nginx-prometheus-exporter.env`. It scrapes the loopback-only stub-status endpoint and exposes metrics on loopback port `9113`.
- OpenResty access logs include request ID, latency, cache status, upstream timing, and response size for downstream aggregation.
