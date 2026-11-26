FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /framegrab ./cmd/framegrab

# Alpine's ffmpeg carries zscale (HDR tonemapping) and libwebp.
FROM alpine:3.23
LABEL org.opencontainers.image.source=https://github.com/Pale-whale/framegrab
RUN apk add --no-cache ffmpeg && adduser -D -H -u 1000 framegrab
COPY --from=build /framegrab /usr/local/bin/framegrab
USER 1000
EXPOSE 8080
ENTRYPOINT ["framegrab"]
