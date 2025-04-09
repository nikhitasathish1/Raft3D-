FROM golang:1.21 AS builder

WORKDIR /app

# First copy go.mod and go.sum to cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -o raft3d .

FROM alpine:latest
RUN apk --no-cache add ca-certificates

WORKDIR /root/
COPY --from=builder /app/raft3d .

EXPOSE 8000 7000
CMD ["./raft3d"]