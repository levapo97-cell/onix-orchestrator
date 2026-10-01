# Multi-stage: compila estático y corre en distroless. El host NO necesita Go.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/onix-orchestrator ./cmd/onix-orchestrator

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/onix-orchestrator /onix-orchestrator
EXPOSE 8084 8085
USER nonroot:nonroot
# Healthcheck contra el puerto de control (:8085).
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/onix-orchestrator", "-healthcheck"]
ENTRYPOINT ["/onix-orchestrator"]
