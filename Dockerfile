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
# No path in the command lines: the config is the default, /etc/nodeprobe/nodeprobe.yaml. Synology's
# Security Advisor (ruleDB Malware/Binary/lock3dCheck.py) SIGSTOPs every process whose command line
# contains "/etc/node" (a backdoor's path, matched without a boundary): it froze nodeprobe, watchdog
# and worker together, at each weekly scan (cmd/nodeprobe/dockerfile_test.go checks the lines here).
HEALTHCHECK --interval=1m --timeout=10s --start-period=1m CMD ["/nodeprobe", "-healthcheck"]
ENTRYPOINT ["/nodeprobe"]
