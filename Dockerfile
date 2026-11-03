FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app/api ./cmd/api \
 && CGO_ENABLED=0 go build -o /app/worker ./cmd/worker

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /app/api /app/api
COPY --from=build /app/worker /app/worker
COPY --from=web /src/web/dist /app/web/dist
ENV DATA_DIR=/data PORT=8080 WEB_DIR=/app/web/dist
EXPOSE 8080
CMD ["/app/api"]
