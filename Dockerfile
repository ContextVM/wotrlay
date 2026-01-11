# Multi-stage build for optimal image size
FROM golang:1.24.1-alpine AS builder

# Set working directory
WORKDIR /app

# Copy go mod files first for better layer caching
COPY go.mod go.sum ./

# Download dependencies (cached layer)
RUN go mod download

# Copy only necessary source files (not entire directory)
COPY *.go ./

# Build the application with stripped binary for smaller size
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o wotrlay .

# Final stage: minimal runtime image
FROM alpine:3.20

# Install ca-certificates for HTTPS requests
RUN apk --no-cache add ca-certificates

# Create non-root user
RUN addgroup -g 1001 -S appgroup && \
    adduser -u 1001 -S appuser -G appgroup

# Set working directory
WORKDIR /app

# Copy binary from builder stage
COPY --from=builder /app/wotrlay .

# Create data directory for Badger DB (will be in /app/badger due to WORKDIR)
RUN mkdir -p /app/badger && \
    chown -R appuser:appgroup /app/badger

# Switch to non-root user
USER appuser

# Expose port
EXPOSE 3334

# Volume for persistent data
VOLUME ["/app/badger"]

# Run the application
CMD ["./wotrlay"]