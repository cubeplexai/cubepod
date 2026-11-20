FROM ubuntu:24.04@sha256:2198e4c65076e9cb0b3d695219df667c0e3b0072da64ca3c2648aca17dcecaf5
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY cubepod-gateway cubepod-controller cubepod-egress /usr/local/bin/
# Infrastructure image only; user images remain free of runtime binaries.
COPY podd podd-init /opt/cubepod/dist/

COPY gateway-entrypoint.sh /usr/local/bin/gateway-entrypoint.sh
