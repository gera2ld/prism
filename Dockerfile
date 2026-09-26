# syntax=docker/dockerfile:1
# $BUILDPLATFORM keeps the toolchain stage on the builder's native arch so the
# Go compile cross-compiles natively instead of running under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Pure-Go SQLite (modernc) needs no CGO. TARGETOS/TARGETARCH are supplied by
# BuildKit; declaring them as ARGs is what puts them in scope here.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/prism .

FROM alpine:3.21
# Outbound HTTPS to upstream providers needs CA certs; tzdata for log timestamps.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -h /app appuser
WORKDIR /app
COPY --from=builder /out/prism /app/prism
RUN mkdir -p /app/pb_data && chown -R appuser:appuser /app
USER appuser
VOLUME /app/pb_data
EXPOSE 8090
# GATEWAY_ENCRYPTION_KEY must be provided at runtime (-e or --env-file).
ENTRYPOINT ["/app/prism"]
CMD ["serve", "--http=0.0.0.0:8090", "--dir=/app/pb_data"]
