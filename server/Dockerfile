FROM node:24-alpine AS web
WORKDIR /src/clients/web
COPY clients/web/package.json clients/web/package-lock.json ./
RUN npm ci --no-fund
COPY clients/web/ ./
RUN npm run build

FROM golang:1.26.6-alpine AS build
WORKDIR /src/server
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
COPY --from=web /src/clients/web/dist/ ./internal/webassets/dist/
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -tags web_release -trimpath -ldflags="-s -w" -o /out/edu-agentd ./cmd/edu-agentd

FROM alpine:3.22
RUN apk add --no-cache postgresql15-client \
    && addgroup -S -g 10001 eduagent \
    && adduser -S -D -H -u 10001 -G eduagent eduagent \
    && install -d -o eduagent -g eduagent -m 0700 /var/lib/edu-agent \
    && install -d -o eduagent -g eduagent -m 0700 /var/lib/edu-agent/nocturne-backups
COPY --from=build /out/edu-agentd /usr/local/bin/edu-agentd
USER 10001:10001
ENV IMPORT_JOB_STAGING_DIR=/var/lib/edu-agent/import-jobs
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/livez || exit 1
ENTRYPOINT ["/usr/local/bin/edu-agentd"]
CMD ["serve"]
