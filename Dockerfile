FROM node:24-alpine AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /paper-digest ./cmd/paper-digest

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata poppler-utils && addgroup -S app && adduser -S -G app app && mkdir -p /data && chown app:app /data
USER app
WORKDIR /app
COPY --from=build /paper-digest /usr/local/bin/paper-digest
COPY --from=frontend /src/web/dist /app/web/dist
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/paper-digest", "health"]
ENTRYPOINT ["/usr/local/bin/paper-digest"]
CMD ["serve", "--listen", ":8080", "--web-dir", "/app/web/dist"]
