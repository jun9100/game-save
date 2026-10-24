# SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
# SPDX-License-Identifier: GPL-3.0-or-later

FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN go vet ./... \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/game-save .

FROM alpine:3.22
ARG VERSION=dev
LABEL org.opencontainers.image.title="game-save" \
      org.opencontainers.image.description="Self-hosted encrypted game-save sync service for Eden and OpenSave" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="GPL-3.0-or-later" \
      org.opencontainers.image.source="https://github.com/jun9100/game-save"
RUN adduser -D -H -u 10001 gamesave \
    && mkdir -p /var/lib/game-save \
    && chown gamesave:gamesave /var/lib/game-save
COPY --from=build /out/game-save /usr/local/bin/game-save
ENV GAME_SAVE_DATA=/var/lib/game-save \
    GAME_SAVE_LISTEN=:8787
USER gamesave
EXPOSE 8787
VOLUME ["/var/lib/game-save"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/game-save", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/game-save"]
