FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /switchyard ./cmd/switchyard

FROM scratch
COPY --from=build /switchyard /switchyard
HEALTHCHECK --interval=10s --timeout=6s --start-period=5s CMD ["/switchyard", "health"]
ENTRYPOINT ["/switchyard"]
