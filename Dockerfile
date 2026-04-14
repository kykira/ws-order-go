# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Install dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
# CGO_ENABLED=0 ensures a static binary, which is required for the scratch image
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /app/wsorder-server ./cmd/server

# Final stage
FROM alpine:latest

WORKDIR /app

# Install ca-certificates for HTTPS requests
RUN apk --no-cache add ca-certificates tzdata

# Copy the binary from the builder stage
COPY --from=builder /app/wsorder-server /app/wsorder-server

# Copy the web directory (required for the UI)
COPY --from=builder /app/web /app/web

# Create a data directory for logs
RUN mkdir -p /app/data && chmod 777 /app/data

# Expose the default port
EXPOSE 9000

# Set default environment variables
ENV WSORDER_PORT=9000

# Run the binary
CMD ["/app/wsorder-server"]