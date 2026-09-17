# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Pure-Go SQLite (modernc) needs no CGO.
RUN CGO_ENABLED=0 go build -trimpath -o /out/prism .

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
