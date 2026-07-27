# syntax=docker/dockerfile:1.7

ARG POSTGRES_IMAGE=postgres:17.10-alpine3.24@sha256:742f40ea20b9ff2ff31db5458d127452988a2164df9e17441e191f3b72252193

FROM ${POSTGRES_IMAGE} AS pgvector-builder

ARG PGVECTOR_VERSION=0.8.5
ARG PGVECTOR_SHA256=6f88a5cbdde31666f4b6c1a6b75c51dcbeffe58f9a7d2b26e502d5a6e5e14d44

RUN apk add --no-cache build-base clang21 llvm21-dev
ADD --checksum=sha256:${PGVECTOR_SHA256} \
    https://github.com/pgvector/pgvector/archive/refs/tags/v${PGVECTOR_VERSION}.tar.gz \
    /tmp/pgvector.tar.gz
RUN mkdir /tmp/pgvector /tmp/pgvector-install && \
    tar -xzf /tmp/pgvector.tar.gz -C /tmp/pgvector --strip-components=1 && \
    make -C /tmp/pgvector OPTFLAGS="" && \
    make -C /tmp/pgvector DESTDIR=/tmp/pgvector-install install

FROM ${POSTGRES_IMAGE} AS runtime

RUN apk add --no-cache su-exec && \
    cp /sbin/su-exec /usr/local/bin/gosu
COPY --from=pgvector-builder /tmp/pgvector-install/ /

FROM scratch

COPY --from=runtime / /

LABEL org.opencontainers.image.source="https://github.com/codefly-dev/service-postgres"

ENV LANG=en_US.utf8
ENV PG_MAJOR=17
ENV PG_VERSION=17.10
ENV PGDATA=/var/lib/postgresql/data

VOLUME ["/var/lib/postgresql/data"]
ENTRYPOINT ["docker-entrypoint.sh"]
STOPSIGNAL SIGINT
EXPOSE 5432
CMD ["postgres"]
