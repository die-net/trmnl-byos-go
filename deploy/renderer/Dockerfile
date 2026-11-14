# The renderer: trmnlp, which renders TRMNL recipes with Firefox, plus the
# trmnl-render shim that the server sends build jobs to. Build from the
# repository root:
#
#   docker build -f deploy/renderer/Dockerfile -t trmnl-byos-render .
#
# Run it with an init process (docker run --init, or init: true in Compose).
# Firefox leaves helper processes that the shim, as process 1, would never
# reap.

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/trmnl-render ./cmd/trmnl-render

# Pinned: trmnlp's command line and project layout are its only interface.
FROM trmnl/trmnlp:v0.34.1
COPY --from=build /out/trmnl-render /usr/local/bin/trmnl-render
# Builds run as an unprivileged user, in per-build directories under /tmp.
RUN useradd --system --create-home --uid 10001 render
USER render
WORKDIR /home/render
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/trmnl-render"]
