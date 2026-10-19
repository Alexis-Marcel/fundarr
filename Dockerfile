# syntax=docker/dockerfile:1
# Fundarr: one static binary that also serves the built UI.
# Front and binary are built on the build machine (cross-compiled, no emulation).

FROM --platform=$BUILDPLATFORM node:24-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS server
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web/embed.go ./web/
COPY --from=web /web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/fundarr ./cmd/fundarr

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=server /out/fundarr /fundarr
ENV FUNDARR_ADDR=:9494 FUNDARR_DATA=/data
VOLUME ["/data"]
EXPOSE 9494
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/fundarr", "health"]
USER nonroot:nonroot
ENTRYPOINT ["/fundarr"]
