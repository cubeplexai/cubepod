FROM golang:1.26-bookworm AS builder
WORKDIR /src
COPY go.mod ./
COPY api ./api
COPY pkg ./pkg
COPY cmd ./cmd
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/podd ./cmd/podd

FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl e2fsprogs util-linux iproute2 git python3 python3-pip python3-venv \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 1000 -s /bin/bash cubeplex \
    && mkdir -p /workspace && chown 1000:1000 /workspace

COPY --from=builder /out/podd /usr/local/bin/podd
WORKDIR /workspace
EXPOSE 49983
ENTRYPOINT ["/usr/local/bin/podd"]
