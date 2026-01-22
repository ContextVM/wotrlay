# Multi-stage build for optimal image size
FROM golang:1.25.5-alpine AS builder

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
FROM scratch

# Set working directory
WORKDIR /app

# Copy binary from builder stage
COPY --from=builder /app/wotrlay .

# Expose port
EXPOSE 3334

# Volume for persistent data
VOLUME ["/app/badger"]

# Run the application
CMD ["./wotrlay"]