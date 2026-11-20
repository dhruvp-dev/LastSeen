# Stage 1: Build binary
FROM golang:1.22-alpine AS builder

WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build static binary without CGO
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bin/lastseen ./cmd/lastseen

# Stage 2: Minimal runtime image
FROM alpine:3.20

# ca-certificates are required for TLS certificate verification during checks and Turso connection
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
RUN mkdir -p /app/bin

COPY --from=builder /bin/lastseen /app/bin/lastseen
COPY --from=builder /bin/lastseen /usr/local/bin/lastseen

# Run as non-privileged user
USER 1000:1000

CMD ["bin/lastseen", "serve"]
