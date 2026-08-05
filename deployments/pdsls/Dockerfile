FROM docker.io/oven/bun:1-debian@sha256:9dba1a1b43ce28c9d7931bfc4eb00feb63b0114720a0277a8f939ae4dfc9db6f AS build

WORKDIR /app
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git patch \
    && rm -rf /var/lib/apt/lists/*

RUN git init \
    && git remote add origin https://github.com/notjuliet/pdsls.git \
    && git fetch --depth 1 origin c98effca13ba34feec77de8ee9aef27909244366 \
    && git checkout --detach FETCH_HEAD \
    && test "$(git rev-parse HEAD)" = "c98effca13ba34feec77de8ee9aef27909244366"

RUN bun install --frozen-lockfile

COPY plc-default.patch /tmp/plc-default.patch
RUN patch -p1 < /tmp/plc-default.patch

ENV APP_DOMAIN=localhost:2584 \
    APP_PROTOCOL=http \
    VITE_DEFAULT_PLC_DIRECTORY=http://localhost:2582
RUN bun run build

FROM docker.io/nginx:1-alpine@sha256:4a73073bd557c65b759505da037898b61f1be6cbcc3c2c3aeac22d2a470c1752

COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
