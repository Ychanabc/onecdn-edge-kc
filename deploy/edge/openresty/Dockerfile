ARG NGINX_VERSION=1.27.1
ARG ALPINE_VERSION=3.20

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS agent-builder

ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY services/edge-agent/go.mod ./
COPY services/edge-agent/ ./
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/edge-agent \
      ./cmd/edge-agent

# OpenResty releases do not currently ship the Nginx HTTP/3 module. Build the
# upstream Nginx QUIC implementation and the OpenResty-compatible Lua modules
# together so Lua, ModSecurity and stream remain available in one runtime.
FROM alpine:${ALPINE_VERSION} AS runtime-builder

ARG NGINX_VERSION
ARG LIBMODSECURITY_VERSION=v3.0.14
ARG MODSECURITY_NGINX_VERSION=v1.0.4
ARG OWASP_CRS_VERSION=v4.16.0
ARG LUAJIT_VERSION=v2.1-20250117
ARG NDK_VERSION=v0.3.3
ARG LUA_NGINX_MODULE_VERSION=v0.10.28
ARG LUA_RESTY_CORE_VERSION=v0.1.31
ARG LUA_RESTY_LRUCACHE_VERSION=v0.15

RUN apk add --no-cache \
      autoconf \
      automake \
      build-base \
      curl \
      curl-dev \
      git \
      libtool \
      linux-headers \
      lmdb-dev \
      libxml2-dev \
      openssl-dev \
      pcre-dev \
      pcre2-dev \
      yajl-dev \
      zlib-dev

RUN git clone --depth 1 --branch "${LIBMODSECURITY_VERSION}" \
      https://github.com/owasp-modsecurity/ModSecurity.git /tmp/ModSecurity \
    && cd /tmp/ModSecurity \
    && git submodule update --init --depth 1 \
    && ./build.sh \
    && ./configure --prefix=/usr/local/modsecurity --disable-doxygen-doc --disable-examples \
    && make -j"$(getconf _NPROCESSORS_ONLN)" \
    && make install

RUN git clone --depth 1 --branch "${MODSECURITY_NGINX_VERSION}" \
      https://github.com/owasp-modsecurity/ModSecurity-nginx.git /tmp/ModSecurity-nginx \
    && git clone --depth 1 --branch "${LUAJIT_VERSION}" \
      https://github.com/openresty/luajit2.git /tmp/luajit2 \
    && make -C /tmp/luajit2 PREFIX=/usr/local/openresty/luajit install \
    && git clone --depth 1 --branch "${NDK_VERSION}" \
      https://github.com/vision5/ngx_devel_kit.git /tmp/ngx_devel_kit \
    && git clone --depth 1 --branch "${LUA_NGINX_MODULE_VERSION}" \
      https://github.com/openresty/lua-nginx-module.git /tmp/lua-nginx-module \
    && git clone --depth 1 --branch "${LUA_RESTY_CORE_VERSION}" \
      https://github.com/openresty/lua-resty-core.git /tmp/lua-resty-core \
    && git clone --depth 1 --branch "${LUA_RESTY_LRUCACHE_VERSION}" \
      https://github.com/openresty/lua-resty-lrucache.git /tmp/lua-resty-lrucache \
    && curl -fsSLo /tmp/nginx.tar.gz "https://nginx.org/download/nginx-${NGINX_VERSION}.tar.gz" \
    && tar -xzf /tmp/nginx.tar.gz -C /tmp \
    && cd "/tmp/nginx-${NGINX_VERSION}" \
    && export LUAJIT_LIB=/usr/local/openresty/luajit/lib \
    && export LUAJIT_INC=/usr/local/openresty/luajit/include/luajit-2.1 \
    && ./configure \
      --prefix=/usr/local/openresty/nginx \
      --sbin-path=/usr/local/openresty/nginx/sbin/nginx \
      --conf-path=/usr/local/openresty/nginx/conf/nginx.conf \
      --pid-path=/tmp/openresty.pid \
      --error-log-path=/dev/stderr \
      --http-log-path=/dev/stdout \
      --with-cc-opt="-I${LUAJIT_INC}" \
      --with-ld-opt="-L${LUAJIT_LIB} -Wl,-rpath,/usr/local/openresty/luajit/lib" \
      --with-http_ssl_module \
      --with-http_v2_module \
      --with-http_v3_module \
      --with-http_realip_module \
      --with-http_stub_status_module \
      --with-http_gzip_static_module \
      --with-http_slice_module \
      --with-stream \
      --with-stream_ssl_module \
      --with-stream_ssl_preread_module \
      --add-module=/tmp/ngx_devel_kit \
      --add-module=/tmp/lua-nginx-module \
      --add-module=/tmp/ModSecurity-nginx \
    && make -j"$(getconf _NPROCESSORS_ONLN)" \
    && make install \
    && make -C /tmp/lua-resty-core install PREFIX=/usr/local/openresty \
    && make -C /tmp/lua-resty-lrucache install PREFIX=/usr/local/openresty \
    && git clone --depth 1 --branch "${OWASP_CRS_VERSION}" \
      https://github.com/coreruleset/coreruleset.git /tmp/owasp-crs

FROM alpine:${ALPINE_VERSION}

RUN apk add --no-cache \
      ca-certificates \
      curl \
      libgcc \
      libstdc++ \
      libxml2 \
      lmdb \
      pcre \
      pcre2 \
      yajl \
      zlib \
    && mkdir -p \
      /etc/openresty/conf.d \
      /etc/openresty/modsecurity/custom \
      /etc/openresty/modsecurity/owasp-crs \
      /etc/openresty/stream.d \
      /usr/local/openresty/bin \
      /usr/local/share/cdn-edge/bootstrap/conf/sites \
      /usr/local/share/cdn-edge/bootstrap/conf/stream/services \
      /usr/local/share/cdn-edge/bootstrap/logs \
      /usr/local/share/cdn-edge/bootstrap/cache \
      /var/cache/openresty \
      /var/log/openresty \
    && chown -R nobody:nobody /var/cache/openresty /var/log/openresty

COPY --from=runtime-builder /usr/local/openresty /usr/local/openresty
COPY --from=runtime-builder /usr/local/modsecurity/lib/libmodsecurity.so* /usr/local/lib/
COPY --from=runtime-builder /tmp/owasp-crs/rules /etc/openresty/modsecurity/owasp-crs/rules
COPY --from=runtime-builder /tmp/owasp-crs/crs-setup.conf.example /etc/openresty/modsecurity/owasp-crs/crs-setup.conf

RUN ln -s ../nginx/sbin/nginx /usr/local/openresty/bin/openresty

COPY deploy/edge/openresty/nginx.conf /usr/local/openresty/nginx/conf/nginx.conf
COPY deploy/edge/openresty/conf.d/ /etc/openresty/conf.d/
COPY deploy/edge/openresty/modsecurity/main.conf /etc/openresty/modsecurity/main.conf
COPY deploy/edge/openresty/modsecurity/custom/ /etc/openresty/modsecurity/custom/
COPY deploy/edge/openresty/trusted-proxies.conf /etc/openresty/trusted-proxies.conf
COPY deploy/edge/bootstrap/ /usr/local/share/cdn-edge/bootstrap/
COPY deploy/edge/bin/ /usr/local/bin/
COPY --from=agent-builder /out/edge-agent /usr/local/bin/edge-agent

ENV LD_LIBRARY_PATH=/usr/local/lib:/usr/local/openresty/luajit/lib

RUN chmod 0555 \
      /usr/local/bin/edge-agent \
      /usr/local/bin/edge-agent-entrypoint \
      /usr/local/bin/edge-data-init \
    && find /usr/local/share/cdn-edge/bootstrap -type d -exec chmod 0755 {} + \
    && find /usr/local/share/cdn-edge/bootstrap -type f -exec chmod 0644 {} + \
    && /usr/local/openresty/bin/openresty -V 2>&1 | grep -q -- '--with-http_v3_module' \
    && /usr/local/openresty/bin/openresty -t

# HTTP/3 uses the same public port over UDP; L4 services may still use any other host port.
EXPOSE 8080 443/tcp 443/udp 8443/tcp 8443/udp
USER nobody

HEALTHCHECK --interval=15s --timeout=3s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/openresty/bin/openresty"]
CMD ["-g", "daemon off;"]
