# syntax=docker/dockerfile:1.7

FROM node:22-slim AS frontend
WORKDIR /app
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm ci
COPY frontend/ .
RUN npm run build

FROM golang:1.26.3 AS build

ARG DOCTOR_SERVICE=doctor-ingest
ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
ARG BUILD_TIME=unknown
WORKDIR /src

COPY go.mod go.sum ./
COPY --from=monofs go.mod /monofs/go.mod
RUN go mod download

COPY . .
COPY --from=monofs . /monofs
COPY --from=frontend /internal/query/ui/dist ./internal/query/ui/dist
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
	-trimpath \
	-ldflags "-s -w -X github.com/rydzu/ainfra/doctor/internal/buildinfo.Version=${BUILD_VERSION} -X github.com/rydzu/ainfra/doctor/internal/buildinfo.Commit=${BUILD_COMMIT} -X github.com/rydzu/ainfra/doctor/internal/buildinfo.BuildTime=${BUILD_TIME}" \
	-o /out/doctor ./cmd/${DOCTOR_SERVICE}

FROM gcr.io/distroless/base-debian12

ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
ARG BUILD_TIME=unknown

LABEL org.opencontainers.image.version=${BUILD_VERSION}
LABEL org.opencontainers.image.revision=${BUILD_COMMIT}
LABEL org.opencontainers.image.created=${BUILD_TIME}

COPY --from=build /out/doctor /doctor
ENTRYPOINT ["/doctor"]
