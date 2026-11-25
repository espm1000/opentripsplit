# syntax=docker/dockerfile:1

FROM golang:1.27.4@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The version comes from .release-please-manifest.json. A branch build
# passes its short commit SHA here to show that instead; releases leave it empty.
ARG COMMIT=""
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/espm1000/opentripsplit.commit=${COMMIT}" -o /out/opentripsplit ./cmd/opentripsplit \
 && mkdir -p /out/data

# Distroless: no shell, no package manager. Runs as uid 65532 (nonroot).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
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
