# --- Build Stage ---
FROM golang:1.25-alpine AS builder

# Install build dependencies. CGO is needed for pgx.
RUN apk add --no-cache git build-base

WORKDIR /app

# 1. OPTIMIZATION: Copy dependencies first
COPY go.mod go.sum ./

# 2. OPTIMIZATION: Download modules.
# This layer will be cached unless go.mod/go.sum changes.
RUN go mod download

# Copy the rest of the source code.
COPY . .

# Build the application.
# -ldflags '-w -s': strips debug information to reduce binary size
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags '-w -s' -o /app/server ./

# --- Final Stage ---
FROM alpine:latest

# 3. LOCALIZATION & UTILS: Add SSL, Timezone data, and curl (for healthcheck)
RUN apk add --no-cache ca-certificates curl tzdata

# Set Timezone to Jakarta (WIB)
ENV TZ=Asia/Jakarta

# 4. SECURITY: Create a non-root user
# -D: Don't assign a password
# -g: Add to group 'appuser'
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app

# Copy the built application binary from the builder stage.
COPY --from=builder /app/server /app/server

# Change ownership of the app directory to the non-root user
RUN chown -R appuser:appgroup /app

# 5. SECURITY: Switch to non-root user
USER appuser

# Expose the port
EXPOSE 8080

CMD ["/app/server"]