# Multi-stage Dockerfile for cubepod-volume-nfs
FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY api ./api
COPY cmd ./cmd
COPY pkg ./pkg

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bin/cubepod-volume-nfs ./cmd/cubepod-volume-nfs

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 1000 cubeplex \
    && adduser -D -u 1000 -G cubeplex cubeplex

COPY --from=builder /bin/cubepod-volume-nfs /usr/local/bin/cubepod-volume-nfs

USER 1000:1000

EXPOSE 2049 8080

ENTRYPOINT ["/usr/local/bin/cubepod-volume-nfs"]
