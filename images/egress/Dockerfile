# Multi-stage Dockerfile for cubepod-egress
FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY go.mod ./
COPY api ./api
COPY cmd ./cmd
COPY pkg ./pkg

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bin/cubepod-egress ./cmd/cubepod-egress

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata nftables su-exec

COPY --from=builder /bin/cubepod-egress /usr/local/bin/cubepod-egress
COPY images/egress/egress-init.sh /usr/local/bin/egress-init.sh
RUN chmod +x /usr/local/bin/egress-init.sh

EXPOSE 18080 18081 18443 18053 19090

ENTRYPOINT ["/usr/local/bin/egress-init.sh"]
