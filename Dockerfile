FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
# 版本号由 CI 传入，保证容器内 /healthz 自报的版本与 tag 一致
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/meoo2api ./cmd/server

FROM alpine:3.20
# 零 apk 依赖：adduser/addgroup 来自 busybox，证书从构建阶段复制，
# 时区数据已由 time/tzdata 编进二进制。少一次网络拉包就少一种构建失败面。
RUN addgroup -g 10001 app && adduser -D -u 10001 -G app app
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/meoo2api /usr/local/bin/meoo2api
USER 10001:10001
WORKDIR /data
ENV DATA_DIR=/data BIND=0.0.0.0 PORT=8791 TZ=Asia/Shanghai
VOLUME ["/data"]
EXPOSE 8791
ENTRYPOINT ["meoo2api"]
