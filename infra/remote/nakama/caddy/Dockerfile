# Custom Caddy build with the rate-limit plugin.
# Released-Caddy-equivalent + mholt/caddy-ratelimit.
#
# Rebuild whenever Caddy ships a new release: bump the FROM
# tag below, redeploy the nakama compose stack.

FROM caddy:2-builder AS builder
RUN xcaddy build \
    --with github.com/mholt/caddy-ratelimit

FROM caddy:2-alpine
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
