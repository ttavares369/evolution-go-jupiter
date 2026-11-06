FROM golang:1.25.0-alpine AS build

RUN apk update && apk add --no-cache git build-base libjpeg-turbo-dev libwebp-dev ca-certificates curl

WORKDIR /build

# Base exata usada em produção antes do patch.
RUN git clone --depth 1 --branch 0.7.2 https://github.com/evolution-foundation/evolution-go.git . \
    && git rev-parse HEAD | grep -qx 9337afc47e10b86cc896a6f432240e40fee95dd1

# PR #154: estabiliza lifecycle, reconexão, QR, restauração de sessões
# e fecha corretamente os sqlstore containers durante reinícios controlados.
COPY patches/pr154.patch /tmp/pr154.patch
RUN printf '%s  %s\n' fcccb9f9ad5cd40332fd96a72d41f954e7c588e5c585137d648d6996e5c628a5 /tmp/pr154.patch | sha256sum -c - \
    && mkdir /tmp/pr154 \
    && git mailsplit -o/tmp/pr154 /tmp/pr154.patch \
    && git apply --check /tmp/pr154/0001 && git apply /tmp/pr154/0001 \
    && git apply --check /tmp/pr154/0002 && git apply /tmp/pr154/0002 \
    && git apply --check /tmp/pr154/0003 && git apply /tmp/pr154/0003

COPY patches/lembrai-webhook.patch /tmp/lembrai-webhook.patch
RUN printf '%s  %s\n' c163460384386317330b4420d41f8ff67a9874e01fca247624ed9f8a5b02921d /tmp/lembrai-webhook.patch | sha256sum -c - \
    && git apply --check /tmp/lembrai-webhook.patch \
    && git apply /tmp/lembrai-webhook.patch

# PR #229 handshake-version fix, retaining the PR #154 QR/session lifecycle.
COPY patches/client-version.patch /tmp/client-version.patch
RUN printf '%s  %s\n' dd6044e185c36513c72bedda5cf6cc8448de63562dc0a0ca3f4fbc5d9c8519a7 /tmp/client-version.patch | sha256sum -c - \
    && git apply --check /tmp/client-version.patch \
    && git apply /tmp/client-version.patch

# Immutable source baseline for the legacy AST/byte-identity regression proof.
RUN cp -a /build /tmp/queue-v1-legacy-baseline
COPY patches/credential-logs.patch /tmp/credential-logs.patch
RUN printf '%s  %s\n' c746a83caf424ebbcc35eba867919466a66fc4b5035bdfb4c4a00e600eb3e787 /tmp/credential-logs.patch | sha256sum -c - \
    && git apply --check /tmp/credential-logs.patch \
    && git apply /tmp/credential-logs.patch

COPY overlay/ /build/
COPY patches/queue-v1.patch /tmp/queue-v1.patch
RUN printf '%s  %s\n' 22a65d1172c9c13d6b3a35b9c99366e68fc0d278b058d8c4647a70c538d29d43 /tmp/queue-v1.patch | sha256sum -c - \
    && git apply --check /tmp/queue-v1.patch \
    && git apply /tmp/queue-v1.patch
# Persistent per-UUID gate and advanced-settings scope fix. No commits/index writes.
COPY patches/exclusive-instances.patch /tmp/exclusive-instances.patch
RUN printf '%s  %s\n' cf8ce370d3e54c3386e2dab0f878d0c7285c0c46a304e20724cac842ef8b7427 /tmp/exclusive-instances.patch | sha256sum -c - \
    && git apply --check /tmp/exclusive-instances.patch \
    && git apply /tmp/exclusive-instances.patch
COPY tests/webhook_producer_test.go pkg/events/webhook/webhook_producer_test.go
COPY tests/credential_logs_test.go pkg/config/credential_logs_test.go

# Candidate only: never overwrite earlier production image tags.
RUN printf '%s\n' '0.7.2-jupiter3-queue-v1' > VERSION

RUN GOTOOLCHAIN=local go mod download
# Real PostgreSQL tests are opt-in and run separately with a disposable fixture.
RUN EXCLUSIVE_INSTANCES_PATCH=/tmp/exclusive-instances.patch CREDENTIAL_LOGS_BASELINE=/tmp/queue-v1-legacy-baseline GOTOOLCHAIN=local go test -mod=readonly ./... \
    && GOTOOLCHAIN=local go vet -mod=readonly ./...
RUN CGO_ENABLED=1 GOTOOLCHAIN=local go build -mod=readonly -ldflags "-X main.version=0.7.2-jupiter3-queue-v1" -o server ./cmd/evolution-go

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
