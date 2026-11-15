FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mochi ./cmd/mochi \
    && go clean -cache

FROM alpine:3.23
RUN apk add --no-cache ca-certificates su-exec sqlite \
    && addgroup -g 10001 mochi \
    && adduser -D -u 10001 -G mochi mochi \
    && mkdir /data && chown mochi:mochi /data
WORKDIR /app
COPY --from=build /out/mochi /usr/local/bin/mochi
COPY --chown=10001:10001 configs ./configs
COPY --chmod=755 deploy/entrypoint.sh /usr/local/bin/mochi-entrypoint
ENV PORT=8080 MOCHI_DB_PATH=/data/mochi.db MOCHI_WORKFLOW=configs/workflows/pr-review-small.yaml
EXPOSE 8080
# The entrypoint owns the mounted data directory, then drops to uid/gid 10001
# before validation and server startup. This also handles fresh Render disks.
ENTRYPOINT ["mochi-entrypoint"]
CMD ["serve"]
