# Multi-stage Dockerfile for cubepod-egress
FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY api ./api
COPY cmd ./cmd
COPY pkg ./pkg

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bin/cubepod-egress ./cmd/cubepod-egress

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /bin/cubepod-egress /usr/local/bin/cubepod-egress

EXPOSE 18080

ENTRYPOINT ["/usr/local/bin/cubepod-egress"]
