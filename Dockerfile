FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/meoo2api ./cmd/server

FROM alpine:3.20
RUN addgroup -g 10001 app && adduser -D -u 10001 -G app app \
 && apk add --no-cache ca-certificates tzdata
COPY --from=build /out/meoo2api /usr/local/bin/meoo2api
USER 10001:10001
WORKDIR /data
ENV DATA_DIR=/data BIND=0.0.0.0 PORT=8791 TZ=Asia/Shanghai
VOLUME ["/data"]
EXPOSE 8791
ENTRYPOINT ["meoo2api"]
