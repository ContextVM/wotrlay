# Multi-stage build for optimal image size
FROM golang:1.24.1-alpine AS builder

# Set working directory
WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o wotrlay .

# Final stage: minimal runtime image
FROM alpine:latest

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