FROM ubuntu:24.04@sha256:2198e4c65076e9cb0b3d695219df667c0e3b0072da64ca3c2648aca17dcecaf5
RUN apt-get update && apt-get install -y --no-install-recommends python3 zstd ca-certificates dmsetup thin-provisioning-tools util-linux e2fsprogs && rm -rf /var/lib/apt/lists/*
