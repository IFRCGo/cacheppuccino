# -------- build stage --------
FROM golang:1.23 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/cacheppuccino .

# -------- runtime stage --------
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=build /out/cacheppuccino /cacheppuccino

VOLUME ["/data"]

ENV LISTEN_ADDR=":8080"
EXPOSE 8080

ENTRYPOINT ["/cacheppuccino"]
