# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.25-alpine3.22 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/devenv-gateway ./cmd/devenv-gateway

FROM alpine:3.22
RUN apk add --no-cache --upgrade \
    'libcrypto3>=3.5.8-r0' \
    'libssl3>=3.5.8-r0' \
    ca-certificates-bundle
COPY --from=build /out/devenv-gateway /usr/local/bin/devenv-gateway
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --retries=3 CMD wget -q -O - http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/usr/local/bin/devenv-gateway"]
