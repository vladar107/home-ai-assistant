# syntax=docker/dockerfile:1.7

# Upstream publishes no image, so this builds its source at one reviewed commit. Bump UPSTREAM_SHA
# and UPSTREAM_SHA256 together, in their own commit, after reading the upstream diff.
ARG PYTHON_IMAGE=python:3.12-slim-bookworm@sha256:935c3c5b12357a7e10caa9c1e6ca5692344d97cf229832ef584be2cec9fc03df

FROM ghcr.io/astral-sh/uv:0.13.7@sha256:96f40d3904aad8d0bd010539001d36c8e2188b981e1334471a90c4a1d93bb490 AS uv

FROM ${PYTHON_IMAGE} AS build
ARG UPSTREAM_SHA=6a0232681c2db2e42c9095e140ec420f74652ae3
ARG UPSTREAM_SHA256=bfc0f74b94cd8798867bb1dd4f32be730ab26b9a11935496e8705e1e3d04d634
ADD --checksum=sha256:${UPSTREAM_SHA256} \
    https://github.com/jasp-nerd/marktplaats-mcp/archive/${UPSTREAM_SHA}.tar.gz /tmp/upstream.tar.gz
WORKDIR /app
RUN tar -xzf /tmp/upstream.tar.gz --strip-components=1 -C /app
COPY --from=uv /uv /bin/uv
# Only the hash-locked wheels are installed. The project itself is copied as source below, so no
# build backend, which the lock file does not pin, ever runs.
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=0 UV_NO_BUILD=1
RUN uv sync --frozen --no-dev --no-install-project

FROM ${PYTHON_IMAGE}
COPY --from=build /app/.venv /app/.venv
COPY --from=build /app/src/marktplaats_mcp /app/src/marktplaats_mcp
RUN useradd --no-create-home --uid 1000 mcp
ENV PATH=/app/.venv/bin:$PATH \
    PYTHONPATH=/app/src \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    MALLOC_ARENA_MAX=2
USER mcp
EXPOSE 8000
CMD ["python", "-m", "marktplaats_mcp.server"]
