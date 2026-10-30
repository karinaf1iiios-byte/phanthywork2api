# ---- 构建阶段 ----
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/phanthywork2api ./cmd/server \
 && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/phanthywork2api-login ./cmd/login

# ---- 运行阶段 ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/phanthywork2api /out/phanthywork2api-login /usr/local/bin/
COPY config.example.json /app/config.example.json
RUN mkdir -p /app/auths /app/data && chown -R app:app /app
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["phanthywork2api"]