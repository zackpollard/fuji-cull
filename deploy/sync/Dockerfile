# Build a tiny static fuji-sync image. Pure Go, no cgo -> distroless static.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fuji-sync ./cmd/fuji-sync

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/fuji-sync /fuji-sync
VOLUME ["/data"]
EXPOSE 8777
# SYNC_API_KEY must be provided at runtime; --db defaults to /data/sync.db
ENTRYPOINT ["/fuji-sync"]
