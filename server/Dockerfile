# syntax=docker/dockerfile:1
FROM golang:1.26.6-alpine AS build
WORKDIR /src/server
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/edu-agentd ./cmd/edu-agentd

FROM alpine:3.22
RUN addgroup -S -g 10001 eduagent \
    && adduser -S -D -H -u 10001 -G eduagent eduagent
COPY --from=build /out/edu-agentd /usr/local/bin/edu-agentd
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/livez || exit 1
ENTRYPOINT ["/usr/local/bin/edu-agentd"]
CMD ["serve"]
