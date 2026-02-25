# -------- build stage --------
FROM golang:1.23 AS build

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -trimpath -ldflags="-s -w" -o /out/cacheppuccino .

# -------- runtime stage --------

FROM gcr.io/distroless/static-debian12

# FROM alpine:3.21
# RUN apk add --no-cache ca-certificates && update-ca-certificates

WORKDIR /
COPY --from=build /out/cacheppuccino /cacheppuccino

VOLUME ["/data"]

ENV LISTEN_ADDR=":8080"
EXPOSE 8080

ENTRYPOINT ["/cacheppuccino"]
