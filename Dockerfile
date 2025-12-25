# Stage 1: Build the Go application
FROM golang:1.25-alpine AS builder

# Working directory
WORKDIR /app

# Copy go mod and sum files to cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code
COPY . .

# Build the binary
# CGO_ENABLED=0 ensures a static binary (no external C library dependencies)
RUN CGO_ENABLED=0 GOOS=linux go build -o app main.go

# Stage 2: Create the final minimal image
FROM alpine:latest

WORKDIR /root/

# Install CA certificates (Required for HTTPS calls to Cloudflare R2)
RUN apk --no-cache add ca-certificates

# Copy the binary from the builder stage
COPY --from=builder /app/app .

# Expose the port defined in your Go code
EXPOSE 8081

# Command to run the executable
CMD ["./app"]