FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login-browser .

FROM alpine:3.20
# chromium 之外只装字体：阿里云的登录组件会渲染中文提示，缺字体会变豆腐块。
RUN apk add --no-cache chromium nss freetype harfbuzz ttf-dejavu font-noto-cjk \
 && addgroup -g 10001 app && adduser -D -u 10001 -G app app
COPY --from=build /out/login-browser /usr/local/bin/login-browser
ENV CHROME_PATH=/usr/bin/chromium \
    BIND=0.0.0.0 PORT=8792 \
    HOME=/tmp TMPDIR=/tmp
USER 10001:10001
WORKDIR /tmp
EXPOSE 8792
ENTRYPOINT ["/usr/local/bin/login-browser"]
