FROM --platform=$BUILDPLATFORM node:24.19.0-bookworm-slim@sha256:3638d9a6fe4030bd716be989438248074489337ba3275657f93595428be4fc03 AS web-build
WORKDIR /src/web

COPY web/package.json web/package-lock.json web/.npmrc web/.node-version ./
RUN test "$(node --version)" = "v24.19.0" \
    && test "$(npm --version)" = "11.17.0" \
    && npm ci

COPY web/ ./
RUN npm run check && npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.0-bookworm@sha256:484ef6066fa69acb059fdfeda7ba2b8f7391f2ef6abc6f9b8411e669ebd56466 AS go-source
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0

WORKDIR /src

COPY go.mod go.sum ./
RUN test "$(go env GOVERSION)" = "go1.27.0" \
    && test "$(go env GOTOOLCHAIN)" = "local"
RUN go mod download && go mod verify

COPY cmd ./cmd
COPY configs ./configs
COPY internal ./internal
COPY migrations ./migrations
COPY web/embed.go ./web/embed.go
COPY --from=web-build /src/web/dist ./web/dist

FROM go-source AS agent-build
ARG TARGETOS
ARG TARGETARCH
RUN GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -p=1 -mod=readonly -trimpath -ldflags='-s -w' -o /out/xtunnel-agent ./cmd/agent \
    && install -d -o 65532 -g 65532 -m 0700 /out/runtime/var/lib/xtunnel

FROM go-source AS server-build
ARG TARGETOS
ARG TARGETARCH
RUN GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -p=1 -mod=readonly -trimpath -ldflags='-s -w' -o /out/xtunnel-server ./cmd/server \
    && install -d -o 65532 -g 65532 -m 0700 /out/runtime/run/xtunnel /out/runtime/var/lib/xtunnel

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS agent
COPY --chown=65532:65532 --from=agent-build /out/runtime/var/lib/xtunnel /var/lib/xtunnel
COPY --chown=65532:65532 --from=agent-build /out/xtunnel-agent /usr/local/bin/xtunnel-agent

USER 65532:65532
VOLUME ["/var/lib/xtunnel"]
ENTRYPOINT ["/usr/local/bin/xtunnel-agent"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS server

COPY --chown=65532:65532 --from=server-build /out/runtime/run/xtunnel /run/xtunnel
COPY --chown=65532:65532 --from=server-build /out/runtime/var/lib/xtunnel /var/lib/xtunnel
COPY --chown=65532:65532 --from=server-build /out/xtunnel-server /usr/local/bin/xtunnel-server

USER 65532:65532
VOLUME ["/var/lib/xtunnel"]
ENTRYPOINT ["/usr/local/bin/xtunnel-server"]
