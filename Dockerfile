FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /paper-digest ./cmd/paper-digest

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && addgroup -S app && adduser -S -G app app && mkdir -p /data && chown app:app /data
USER app
COPY --from=build /paper-digest /usr/local/bin/paper-digest
ENV DB_PATH=/data/digest.db TZ=Asia/Shanghai
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/paper-digest", "health"]
ENTRYPOINT ["/usr/local/bin/paper-digest"]
CMD ["serve"]
