# Build stage
FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o octograph-query main.go

# Final image
FROM alpine:latest
WORKDIR /app
RUN apk add --no-cache tzdata
COPY --from=builder /app/octograph-query .
COPY template-default.html .
COPY template-iphone16pro.html .
COPY Inter_18pt-Light.ttf .
COPY Inter_18pt-SemiBold.ttf .
EXPOSE 8080
ENV HTTP_PORT=8080
CMD ["./octograph-query"]

