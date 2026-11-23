FROM ubuntu:24.04@sha256:2198e4c65076e9cb0b3d695219df667c0e3b0072da64ca3c2648aca17dcecaf5
ARG UBUNTU_MIRROR=""
RUN if [ -n "$UBUNTU_MIRROR" ] && [ -f /etc/apt/sources.list.d/ubuntu.sources ]; then \
      sed -i "s|http://archive.ubuntu.com/ubuntu|${UBUNTU_MIRROR}|g" /etc/apt/sources.list.d/ubuntu.sources; \
    fi && \
    printf 'Acquire::Retries "3";\nAcquire::http::Timeout "30";\nAcquire::https::Timeout "30";\n' >/etc/apt/apt.conf.d/99-ci-retries && \
    apt-get update && apt-get install -y --no-install-recommends python3 zstd ca-certificates dmsetup thin-provisioning-tools util-linux e2fsprogs && rm -rf /var/lib/apt/lists/*
