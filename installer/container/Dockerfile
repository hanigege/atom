# The atom image (docs/container.md section 4): the programs of one glibc package on debian:trixie-slim.
# Built by installer/scripts/build-image.sh, which lays the package out in the build context as atom/.
ARG BASE=debian:trixie-slim
FROM ${BASE}
# DEPS=skip (build-image.sh --base): the base already has the packages below
ARG DEPS=install
RUN if [ "$DEPS" = install ]; then \
      apt-get update \
      && apt-get install -y --no-install-recommends nftables iproute2 ca-certificates tzdata \
      && rm -rf /var/lib/apt/lists/*; \
    fi \
    && command -v nft >/dev/null && command -v ip >/dev/null
COPY atom/ /opt/atom/
ENV ATOM_DEPLOYMENT=container
VOLUME /opt/atom/data
STOPSIGNAL SIGTERM
ENTRYPOINT ["/opt/atom/bin/atom-installer", "container"]
