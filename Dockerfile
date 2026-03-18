# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o server-bin ./cmd/server

# Runtime stage
FROM alpine:3.21

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /build/server-bin .
COPY --from=builder /build/PROTOCOL.md .

# Create directories for runtime data and skins
RUN mkdir -p /app/skins /app/data && \
    echo '[]' > /app/skins/manifest.json

EXPOSE 3001

ENTRYPOINT ["./server-bin"]
