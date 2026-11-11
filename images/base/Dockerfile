FROM rust:1.85-bookworm AS builder
WORKDIR /src
COPY Cargo.toml ./
COPY crates ./crates
RUN cargo build --release --workspace \
    && cp target/release/podd-init /usr/local/bin/podd-init \
    && cp target/release/podd /usr/local/bin/podd

FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl e2fsprogs util-linux iproute2 git python3 python3-pip python3-venv \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 1000 -s /bin/bash cubeplex \
    && mkdir -p /workspace && chown 1000:1000 /workspace

COPY --from=builder /usr/local/bin/podd-init /usr/local/bin/podd-init
COPY --from=builder /usr/local/bin/podd /usr/local/bin/podd
WORKDIR /workspace
EXPOSE 49983
ENTRYPOINT ["/usr/local/bin/podd-init", "/usr/local/bin/podd"]
