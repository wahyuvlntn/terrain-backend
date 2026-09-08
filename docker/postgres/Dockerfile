# PostgreSQL 16 (same Alpine/musl image the existing data volume was created with)
# plus pgvector, compiled without -march=native so the image stays portable.
# Never switch to a Debian-based image on the existing volume: collation differs.
ARG PG_BASE=postgres:16-alpine@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea
FROM ${PG_BASE}
LABEL org.opencontainers.image.title="Digital Twin AFDD PostgreSQL"
ARG PGVECTOR_VERSION=0.8.6
RUN set -eux; \
    apk add --no-cache --virtual .vector-build build-base $DOCKER_PG_LLVM_DEPS; \
    wget -qO /tmp/pgvector.tgz "https://github.com/pgvector/pgvector/archive/refs/tags/v${PGVECTOR_VERSION}.tar.gz"; \
    mkdir /tmp/pgvector; tar -xzf /tmp/pgvector.tgz -C /tmp/pgvector --strip-components=1; \
    cd /tmp/pgvector; make OPTFLAGS="" -j"$(nproc)"; make OPTFLAGS="" install; \
    cd /; rm -rf /tmp/pgvector /tmp/pgvector.tgz; apk del --no-network .vector-build; \
    test -f /usr/local/share/postgresql/extension/vector.control
