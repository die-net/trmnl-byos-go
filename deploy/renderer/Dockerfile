# The renderer: trmnlp, which renders TRMNL recipes with Firefox, plus the
# trmnl-render shim that the server sends build jobs to. Build from the
# repository root:
#
#   docker build -f deploy/renderer/Dockerfile -t trmnl-byos-render .
#
# Run it without an init process (no docker run --init, and no init: true in
# Compose). The shim is process 1 and reaps what Firefox leaves behind. An
# init would run as the render user, where no sweep could kill it, so the
# shim refuses to start beside one. Run it with a read-only root file system
# and a tmpfs on /tmp (docker run --read-only --tmpfs /tmp:mode=1777,size=1g,
# or read_only and tmpfs in Compose). The shim refuses to start where its
# user can write anywhere it does not clean after each build, and the image
# leaves /usr/local/bundle and other directories writable.

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
