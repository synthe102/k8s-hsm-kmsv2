# -------------------------------------------------------------------
# Build stage
# -------------------------------------------------------------------
FROM golang:1.26.3-bookworm AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -o kms-plugin ./cmd/kms-plugin

# -------------------------------------------------------------------
# Runtime stage (SoftHSM2 variant – for Proteccio replace the library)
# -------------------------------------------------------------------
FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends softhsm2 && \
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p /var/lib/softhsm/tokens

COPY --from=builder /app/kms-plugin /usr/local/bin/kms-plugin

ENTRYPOINT ["/usr/local/bin/kms-plugin"]
