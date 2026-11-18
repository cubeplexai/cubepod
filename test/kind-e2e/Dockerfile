# Binaries are compiled from this checkout, not downloaded release artifacts.
FROM ubuntu:24.04@sha256:2198e4c65076e9cb0b3d695219df667c0e3b0072da64ca3c2648aca17dcecaf5 AS injector
COPY podd podd-init /opt/cubepod/dist/
FROM ubuntu:24.04@sha256:2198e4c65076e9cb0b3d695219df667c0e3b0072da64ca3c2648aca17dcecaf5 AS control
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY cubepod-gateway cubepod-controller /usr/local/bin/
FROM control AS gateway
ENTRYPOINT ["/usr/local/bin/cubepod-gateway"]
FROM control AS controller
ENTRYPOINT ["/usr/local/bin/cubepod-controller"]
FROM ubuntu:24.04@sha256:2198e4c65076e9cb0b3d695219df667c0e3b0072da64ca3c2648aca17dcecaf5 AS sandbox
RUN apt-get update && apt-get install -y --no-install-recommends python3 curl ca-certificates && rm -rf /var/lib/apt/lists/* \
    && (userdel -r ubuntu || true) && groupadd -g 1000 cubeplex && useradd -m -u 1000 -g 1000 -s /bin/bash cubeplex
# No CubePod binaries: podd and podd-init are injected by the real controller.
FROM sandbox AS sandbox-block
RUN apt-get update && apt-get install -y --no-install-recommends e2fsprogs util-linux && rm -rf /var/lib/apt/lists/*
