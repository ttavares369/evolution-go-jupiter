FROM golang:1.25.0-alpine AS build

RUN apk update && apk add --no-cache git build-base libjpeg-turbo-dev libwebp-dev ca-certificates curl

WORKDIR /build

# Base exata usada em produção antes do patch.
RUN git clone --depth 1 --branch 0.7.2 https://github.com/evolution-foundation/evolution-go.git . \
    && git rev-parse HEAD | grep -qx 9337afc47e10b86cc896a6f432240e40fee95dd1

# PR #154: estabiliza lifecycle, reconexão, QR, restauração de sessões
# e fecha corretamente os sqlstore containers durante reinícios controlados.
COPY patches/pr154.patch /tmp/pr154.patch
RUN git config user.name "Jupiter Build" \
    && git config user.email "build@jupiterti.local" \
    && printf '%s  %s\n' fcccb9f9ad5cd40332fd96a72d41f954e7c588e5c585137d648d6996e5c628a5 /tmp/pr154.patch | sha256sum -c - \
    && git am -3 /tmp/pr154.patch

COPY patches/lembrai-webhook.patch /tmp/lembrai-webhook.patch
RUN git apply --check /tmp/lembrai-webhook.patch \
    && git apply /tmp/lembrai-webhook.patch

COPY tests/webhook_producer_test.go pkg/events/webhook/webhook_producer_test.go
RUN gofmt -w pkg/events/webhook/webhook_producer.go pkg/events/webhook/webhook_producer_test.go

# New version: never overwrite the 0.7.2-jupiter1 production tag.
RUN printf '%s\n' '0.7.2-jupiter2-webhook' > VERSION

RUN go mod download
RUN go test ./pkg/events/webhook
RUN CGO_ENABLED=1 go build -ldflags "-X main.version=0.7.2-jupiter2-webhook" -o server ./cmd/evolution-go

FROM alpine:3.19.1 AS final

# Runtime hardening for Coolify/Alpine:
# - dumb-init is PID 1 and reaps orphaned child processes
# - curl avoids BusyBox wget/ssl_client for HTTP checks
# - bash keeps compatibility with Coolify exec helpers
RUN apk add --no-cache tzdata ffmpeg libjpeg-turbo libwebp poppler-utils curl bash dumb-init

WORKDIR /app

COPY --from=build /build/server ./server
COPY --from=build /build/manager/dist ./manager/dist
COPY --from=build /build/VERSION ./VERSION

ENV TZ=America/Sao_Paulo

EXPOSE 8080

ENTRYPOINT ["/usr/bin/dumb-init", "--", "/app/server"]
