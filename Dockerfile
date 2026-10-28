# syntax=docker/dockerfile:1.7

FROM node:22-slim AS frontend
WORKDIR /app
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm ci
COPY frontend/ .
RUN npm run build

FROM golang:1.26.10 AS build

ARG DOCTOR_SERVICE=doctor-ingest
ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
ARG BUILD_TIME=unknown
WORKDIR /src

COPY go.mod go.sum ./
COPY --from=monofs go.mod /monofs/go.mod
RUN go mod download

# v1.0.1 fixes otelc pinning the pre-rename module path (see monofs/Dockerfile).
ARG OTELC_VERSION=v1.2.0
RUN GOBIN=/usr/local/bin go install go.opentelemetry.io/otelc/tool/cmd/otelc@${OTELC_VERSION}

COPY . .
COPY --from=monofs . /monofs
COPY --from=frontend /internal/query/ui/dist ./internal/query/ui/dist
# Keep -trimpath in GOFLAGS so otelc's nested instrumented builds use the
# same flag set as the top-level build; passing -trimpath only on the command
# line triggers a linker "fingerprint mismatch" for otelc/pkg/runtime.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOFLAGS=-trimpath otelc go build \
	-ldflags "-s -w -X github.com/radryc/doctor/internal/buildinfo.Version=${BUILD_VERSION} -X github.com/radryc/doctor/internal/buildinfo.Commit=${BUILD_COMMIT} -X github.com/radryc/doctor/internal/buildinfo.BuildTime=${BUILD_TIME}" \
	-o /out/doctor ./cmd/${DOCTOR_SERVICE}

FROM gcr.io/distroless/base-debian12
ENV OTEL_GO_DISABLED_INSTRUMENTATIONS=nethttp,grpc

ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
ARG BUILD_TIME=unknown

LABEL org.opencontainers.image.version=${BUILD_VERSION}
LABEL org.opencontainers.image.revision=${BUILD_COMMIT}
LABEL org.opencontainers.image.created=${BUILD_TIME}

COPY --from=build /out/doctor /doctor
ENTRYPOINT ["/doctor"]
