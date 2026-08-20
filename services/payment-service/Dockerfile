# syntax=docker/dockerfile:1.7
# Must be >= the `go` directive in go.mod. It was 1.23 while the modules
# declared 1.25.0, so every Go image failed at `go mod download` — invisible
# locally, where the installed toolchain is newer than either.
FROM golang:1.25-alpine AS build
WORKDIR /src
# Root build context: go.mod has a `replace` pointing at libs/go-modelcheck,
# so it must be inside the context before `go mod download` runs.
COPY libs/go-modelcheck /libs/go-modelcheck
COPY services/payment-service/go.mod services/payment-service/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY services/payment-service/ .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" -o /out/payment-service ./cmd/server

FROM build AS test
RUN CGO_ENABLED=0 go test ./... && go vet ./...

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/payment-service /payment-service
COPY --from=build /src/migrations /migrations
USER 65532:65532
EXPOSE 8086
ENTRYPOINT ["/payment-service"]
