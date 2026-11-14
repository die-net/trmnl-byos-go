# The server: a static binary on a distroless base. Build from the
# repository root:
#
#   docker build -f deploy/server/Dockerfile -t trmnl-byos .
#
# State lives in /srv/data (the database, images and recipes; back this
# up) and /srv/cache (the renderer's asset cache, safe to delete).

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# CGO_ENABLED=0 builds with the pure-Go SQLite driver.
RUN CGO_ENABLED=0 go build -trimpath -o /out/trmnl-byos ./cmd/trmnl-byos
# Empty state directories, owned by the image's user, so new volumes are.
RUN mkdir -p /out/srv/data /out/srv/cache

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/trmnl-byos /usr/local/bin/trmnl-byos
COPY --from=build --chown=65532:65532 /out/srv /srv
WORKDIR /srv
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/trmnl-byos"]
CMD ["serve"]
