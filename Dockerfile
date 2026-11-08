# syntax=docker/dockerfile:1

FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/opentripsplit ./cmd/opentripsplit \
 && mkdir -p /out/data

# Distroless: no shell, no package manager. Runs as uid 65532 (nonroot).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/opentripsplit /opentripsplit
# /data must be writable by uid 65532. A new named volume inherits this
# ownership; for a bind mount, chown the host directory to 65532 yourself.
COPY --from=build --chown=65532:65532 /out/data /data
ENV DB_PATH=/data/opentripsplit.db \
    LISTEN_ADDR=:8080
VOLUME /data
EXPOSE 8080
USER 65532:65532
# No curl in distroless, so the binary checks itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/opentripsplit", "healthcheck"]
ENTRYPOINT ["/opentripsplit"]
