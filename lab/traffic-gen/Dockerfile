FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o traffic-gen .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/traffic-gen /traffic-gen
ENTRYPOINT ["/traffic-gen"]
