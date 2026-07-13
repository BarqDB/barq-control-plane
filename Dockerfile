# Build from the Barq workspace root:
#   docker build -f server/Dockerfile -t barq-server .
FROM golang:1.26.5-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends cmake g++ libssl-dev zlib1g-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /workspace
COPY client/barq-go ./client/barq-go
COPY server ./server
RUN make -C client/barq-go native
WORKDIR /workspace/server
RUN go mod download && CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/barq-server ./cmd/barq-server

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/barq-server /usr/local/bin/barq-server
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/barq-server"]
