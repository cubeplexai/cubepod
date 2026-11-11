# CubePod Runtime Injector Image (`cubepod-podd`)
#
# Packages the tiny, statically-linked Rust binaries (`podd-init` and `podd`) into a minimal
# initContainer image. At Pod startup, `cubepod-controller` runs this initContainer to copy
# `podd-init` and `podd` into an in-memory `emptyDir` mounted at `/opt/cubepod/bin` in the
# user's sandbox container.
#
# This keeps `podd-init` and `podd` 100% decoupled from user sandbox images (any stock OCI
# image like ubuntu:24.04 or cubeplex-sandbox works out of the box without baking in CubePod binaries).

FROM rust:1.85-bookworm AS builder
WORKDIR /src
COPY Cargo.toml Cargo.lock* ./
COPY crates ./crates
RUN cargo build --release --workspace

FROM busybox:1.37-musl
COPY --from=builder /src/target/release/podd-init /opt/cubepod/dist/podd-init
COPY --from=builder /src/target/release/podd /opt/cubepod/dist/podd
CMD ["sh", "-c", "cp -f /opt/cubepod/dist/podd-init /opt/cubepod/dist/podd /opt/cubepod/bin/ && chmod 0755 /opt/cubepod/bin/podd-init /opt/cubepod/bin/podd"]
