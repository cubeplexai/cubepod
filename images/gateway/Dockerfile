# Multi-stage Dockerfile for cubepod-gateway
FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY go.mod ./
COPY api ./api
COPY cmd ./cmd
COPY pkg ./pkg

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bin/cubepod-gateway ./cmd/cubepod-gateway

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /bin/cubepod-gateway /usr/local/bin/cubepod-gateway

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/cubepod-gateway"]
