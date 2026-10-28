# nodeprobe's image: the static binary on distroless (CA certificates, a nonroot user, nothing else).
# Built for each platform from the build platform (pure Go, no emulation).
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$VERSION" -o /nodeprobe ./cmd/nodeprobe

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /nodeprobe /nodeprobe
USER nonroot:nonroot
EXPOSE 9101
HEALTHCHECK --interval=1m --timeout=10s --start-period=1m CMD ["/nodeprobe", "-config", "/etc/nodeprobe/nodeprobe.yaml", "-healthcheck"]
ENTRYPOINT ["/nodeprobe"]
CMD ["-config", "/etc/nodeprobe/nodeprobe.yaml"]
