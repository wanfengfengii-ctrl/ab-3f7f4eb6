# syntax=docker/dockerfile:1

ARG GO_VERSION=1.23

# ---- build: compile the server and smoke binaries ----------------------
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/smoke ./cmd/smoke

# ---- verify: one-shot image that runs build checks, unit tests and the
# ---- API smoke test against the healthy application container ----------
FROM golang:${GO_VERSION}-alpine AS verify
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY scripts ./scripts
COPY --from=build /out/smoke /usr/local/bin/smoke
# Pre-warm the module/build cache; verify.sh re-runs the checks for real.
RUN go build ./... && go vet ./...
CMD ["sh", "scripts/verify.sh"]

# ---- app: minimal runtime image (default target) ------------------------
FROM alpine:3.20 AS app
RUN addgroup -S app && adduser -S -G app app \
 && mkdir -p /data && chown app:app /data
COPY --from=build /out/server /server
USER app
ENV PORT=8080 \
    DATA_DIR=/data
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=12 \
    CMD ["/server", "-healthcheck"]
ENTRYPOINT ["/server"]
