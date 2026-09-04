ARG XTUNNEL_VERSION

FROM --platform=$BUILDPLATFORM node:24.19.0-bookworm-slim@sha256:3638d9a6fe4030bd716be989438248074489337ba3275657f93595428be4fc03 AS web-build
WORKDIR /src/web

COPY web/package.json web/package-lock.json web/.npmrc web/.node-version ./
RUN test "$(node --version)" = "v24.19.0" \
    && test "$(npm --version)" = "11.17.0" \
    && npm ci

COPY web/ ./
RUN npm run check && npm run build

FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS go-source
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0

WORKDIR /src

COPY go.mod go.sum ./
RUN go_version="$(go env GOVERSION)" \
    && case "$go_version" in go1.27.*) go_patch=${go_version#go1.27.} ;; *) exit 1 ;; esac \
    && case "$go_patch" in ''|0*|*[!0-9]*) exit 1 ;; esac \
    && test "$go_patch" -ge 1 \
    && test "$(go env GOTOOLCHAIN)" = "local"
RUN go mod download && go mod verify

COPY cmd ./cmd
COPY configs ./configs
COPY internal ./internal
COPY migrations ./migrations
COPY web/embed.go ./web/embed.go
COPY --from=web-build /src/web/dist ./web/dist

FROM go-source AS versioned-go-source
ARG XTUNNEL_VERSION
RUN case "$XTUNNEL_VERSION" in \
        ''|[!0-9A-Za-z]*|*[!0-9A-Za-z._+-]*) \
            printf '%s\n' 'XTUNNEL_VERSION must be 1-64 characters using letters, digits, dot, underscore, plus, or hyphen' >&2; \
            exit 1 \
            ;; \
    esac \
    && if [ "${#XTUNNEL_VERSION}" -gt 64 ]; then \
        printf '%s\n' 'XTUNNEL_VERSION must be 1-64 characters using letters, digits, dot, underscore, plus, or hyphen' >&2; \
        exit 1; \
    fi

FROM versioned-go-source AS agent-build
ARG TARGETOS
ARG TARGETARCH
ARG XTUNNEL_VERSION
RUN GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -p=1 -mod=readonly -trimpath \
    -ldflags="-s -w -X github.com/lifei6671/xtunnel/internal/buildinfo.version=$XTUNNEL_VERSION" \
    -o /out/xtunnel-agent ./cmd/agent

FROM versioned-go-source AS server-build
ARG TARGETOS
ARG TARGETARCH
ARG XTUNNEL_VERSION
RUN GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -p=1 -mod=readonly -trimpath \
        -ldflags="-s -w -X github.com/lifei6671/xtunnel/internal/buildinfo.version=$XTUNNEL_VERSION" \
        -o /out/xtunnel-server ./cmd/server \
    && install -d -o 65532 -g 65532 -m 0700 \
        /out/runtime/run/xtunnel \
        /out/runtime/var/lib/xtunnel \
        /out/runtime/var/lib/xtunnel/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS agent
COPY --chown=65532:65532 --from=agent-build /out/xtunnel-agent /usr/local/bin/xtunnel-agent

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/xtunnel-agent"]
CMD ["run"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS server

COPY --chown=65532:65532 --from=server-build /out/runtime/run/xtunnel /run/xtunnel
COPY --chown=65532:65532 --from=server-build /out/runtime/var/lib/xtunnel /var/lib/xtunnel
COPY --chown=65532:65532 --from=server-build /out/xtunnel-server /usr/local/bin/xtunnel-server

USER 65532:65532
VOLUME ["/var/lib/xtunnel"]
ENTRYPOINT ["/usr/local/bin/xtunnel-server"]
