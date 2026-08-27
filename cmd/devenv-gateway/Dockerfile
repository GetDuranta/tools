# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.25-alpine3.22 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/devenv-gateway ./cmd/devenv-gateway

FROM alpine:3.22 AS certificates
RUN apk add --no-cache ca-certificates-bundle

FROM scratch
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/devenv-gateway /devenv-gateway
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/devenv-gateway"]
