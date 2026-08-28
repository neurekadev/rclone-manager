# syntax=docker/dockerfile:1.18

FROM --platform=$BUILDPLATFORM golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452 AS build

ARG TARGETOS
ARG TARGETARCH
ARG GIT_TAG=dev
ARG GIT_HASH=unknown

WORKDIR /app

COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -trimpath \
    -ldflags="-s -w -X main.gitTag=$GIT_TAG -X main.gitHash=$GIT_HASH" \
    -o /app/bin/rclone-manager \
    ./cmd/rclone-manager

FROM ubuntu:24.04@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517

ARG GIT_TAG=dev
ARG GIT_HASH=unknown

LABEL org.opencontainers.image.title="rclone-manager" \
      org.opencontainers.image.description="Resilient Docker supervisor for rclone FUSE mounts" \
      org.opencontainers.image.source="https://github.com/neurekadev/rclone-manager" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version=$GIT_TAG \
      org.opencontainers.image.revision=$GIT_HASH

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install --yes --no-install-recommends \
        ca-certificates \
        fuse3 \
        tzdata \
    && sed -i 's/^#user_allow_other/user_allow_other/' /etc/fuse.conf \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /app/bin /app/data /cache /config/rclone /mnt/rclone

COPY --from=build /app/bin/rclone-manager /app/bin/rclone-manager

WORKDIR /app

ENV PATH="/app/data/bin:$PATH" \
    RCLONE_CONFIG=/config/rclone/rclone.conf \
    RCLONE_CACHE_DIR=/cache

VOLUME ["/app/data"]

STOPSIGNAL SIGTERM

ENTRYPOINT ["/app/bin/rclone-manager"]
