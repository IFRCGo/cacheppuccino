# -------- build stage --------
FROM golang:1.23 AS build

ARG VERSION=dev

ARG TARGETOS=linux
ARG TARGETARCH=amd64

ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/cacheppuccino .

# -------- runtime stage --------

FROM gcr.io/distroless/static-debian12

# FROM alpine:3.21
# RUN apk add --no-cache ca-certificates && update-ca-certificates

WORKDIR /
COPY --from=build /out/cacheppuccino /cacheppuccino

# The snapshot cache lives on an emptyDir in Kubernetes and on an anonymous
# volume here; it only has to survive container restarts within a pod.
VOLUME ["/cache"]

ENV LISTEN_ADDR=":8080" \
    INTERNAL_LISTEN_ADDR=":8081" \
    CACHE_DIR="/cache"
EXPOSE 8080 8081

ENTRYPOINT ["/cacheppuccino"]
