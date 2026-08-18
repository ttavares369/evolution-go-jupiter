FROM golang:1.25.0-alpine AS build

RUN apk update && apk add --no-cache git build-base libjpeg-turbo-dev libwebp-dev ca-certificates curl

WORKDIR /build

# Base exata usada em produção antes do patch.
RUN git clone --depth 1 --branch 0.7.2 https://github.com/evolution-foundation/evolution-go.git .

# PR #154: estabiliza lifecycle, reconexão, QR, restauração de sessões
# e fecha corretamente os sqlstore containers durante reinícios controlados.
RUN git config user.name "Jupiter Build" \
    && git config user.email "build@jupiterti.local" \
    && curl -fsSL https://github.com/evolution-foundation/evolution-go/pull/154.patch -o /tmp/pr154.patch \
    && git am -3 /tmp/pr154.patch

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

EXPOSE 8080

ENTRYPOINT ["/app/server"]
