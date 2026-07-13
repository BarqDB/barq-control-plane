# Build from the Barq workspace root:
#   docker build -f server/Dockerfile -t barq-server .
FROM golang:1.26.5-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends cmake g++ libssl-dev zlib1g-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /workspace
COPY client/barq-go ./client/barq-go
RUN cmake -S client/barq-go/external/core -B client/barq-go/external/core/build-go \
      -DCMAKE_BUILD_TYPE=Release \
      -DBARQ_BUILD_LIB_ONLY=ON \
      -DBARQ_ENABLE_SYNC=ON \
      -DBARQ_USE_SYSTEM_OPENSSL=ON \
      -DBARQ_USE_SYSTEM_OPENSSL_PATHS=ON \
    && cmake --build client/barq-go/external/core/build-go --target BarqFFIStatic -j 4
COPY server ./server
WORKDIR /workspace/server
RUN go mod download && CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/barq-server ./cmd/barq-server

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && install -d -o 65532 -g 65532 /var/lib/barq \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/barq-server /usr/local/bin/barq-server
ENV BARQ_LISTEN_ADDR=0.0.0.0:8080 \
    BARQ_DATA_DIR=/var/lib/barq
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=12 \
    CMD curl -fsS http://127.0.0.1:8080/health/ready || exit 1
ENTRYPOINT ["/usr/local/bin/barq-server"]
