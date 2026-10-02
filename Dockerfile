# ---- Build stage ----
FROM golang:1.27-alpine AS builder

WORKDIR /app

# Cache dependencies separately from source changes
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary, no CGO, so it runs on the minimal runtime image below
RUN CGO_ENABLED=0 GOOS=linux go build -o /ticket-service .

# ---- Runtime stage ----
FROM alpine:3.20

RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY --from=builder /ticket-service .

EXPOSE 8080

CMD ["./ticket-service"]