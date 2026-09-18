# syntax=docker/dockerfile:1.7

# Experimental P2 derived worker image. BASE must already exist locally as an
# immutable digest. This is not a complete isthmus runtime release and must not
# be pulled from a mutable tag.
ARG BASE
FROM ${BASE}
COPY --chown=1000:1000 worker /worker
USER 1000:1000
ENTRYPOINT ["/worker"]
