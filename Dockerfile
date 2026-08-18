FROM golang:1.25.0-alpine AS build

RUN apk update && apk add --no-cache git build-base libjpeg-turbo-dev libwebp-dev ca-certificates curl

WORKDIR /build

# Base exata usada em produção antes dos patches.
RUN git clone --depth 1 --branch 0.7.2 https://github.com/evolution-foundation/evolution-go.git .

# PR #154: estabiliza lifecycle, reconexão, QR e restauração de sessões.
RUN curl -fsSL https://github.com/evolution-foundation/evolution-go/pull/154.patch -o /tmp/pr154.patch \
    && git apply --3way /tmp/pr154.patch

# PR #174: reutiliza o pool PostgreSQL compartilhado em StartClient.
RUN curl -fsSL https://github.com/evolution-foundation/evolution-go/pull/174.patch -o /tmp/pr174.patch \
    && git apply --3way /tmp/pr174.patch

# Identificação da nossa build.
RUN printf '%s\n' '0.7.2-jupiter1' > VERSION

RUN go mod download
RUN CGO_ENABLED=1 go build -ldflags "-X main.version=0.7.2-jupiter1" -o server ./cmd/evolution-go

FROM alpine:3.19.1 AS final

RUN apk update && apk add --no-cache tzdata ffmpeg libjpeg-turbo libwebp poppler-utils

WORKDIR /app

COPY --from=build /build/server ./server
COPY --from=build /build/manager/dist ./manager/dist
COPY --from=build /build/VERSION ./VERSION

ENV TZ=America/Sao_Paulo

ENTRYPOINT ["/app/server"]
