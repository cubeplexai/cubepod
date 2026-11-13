# Multi-stage Dockerfile for cubepod-controller
FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY api ./api
COPY cmd ./cmd
COPY pkg ./pkg

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bin/cubepod-controller ./cmd/cubepod-controller

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /bin/cubepod-controller /usr/local/bin/cubepod-controller

EXPOSE 8081

ENTRYPOINT ["/usr/local/bin/cubepod-controller"]
