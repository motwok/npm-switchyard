FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build
ARG TARGETOS TARGETARCH VERSION=dev COVER=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# COVER=1 builds an instrumented binary for the integration tests (writes to $GOCOVERDIR).
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      ${COVER:+-cover -covermode=atomic -coverpkg=./...} \
      -ldflags "-s -w -X main.version=$VERSION" -o /switchyard ./cmd/switchyard

FROM scratch
COPY --from=build /switchyard /switchyard
HEALTHCHECK --interval=10s --timeout=6s --start-period=5s CMD ["/switchyard", "health"]
ENTRYPOINT ["/switchyard"]
