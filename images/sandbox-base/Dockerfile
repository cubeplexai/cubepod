# Example Default Sandbox OS Image (`cubepod-sandbox-base`)
#
# Note: `podd-init` and `podd` are NEVER baked into sandbox images. They are injected at
# container startup via the `cubepod-podd` initContainer (or volume mount in local Docker)
# into `/opt/cubepod/bin`. Any standard Linux OCI image with `e2fsprogs` and `mount` can
# be used as a CubePod sandbox image.

FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl e2fsprogs util-linux iproute2 git python3 python3-pip python3-venv \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 1000 -s /bin/bash cubeplex \
    && mkdir -p /workspace && chown 1000:1000 /workspace

WORKDIR /workspace
