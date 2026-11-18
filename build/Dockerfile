# Build stage
FROM golang:1.24.13-trixie AS builder
WORKDIR /app
RUN go install github.com/matryer/moq@v0.5.3
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/ ./cmd/...

# Prod stage
FROM gcr.io/distroless/static-debian12:nonroot AS prod
COPY --from=builder /out/ /
ENTRYPOINT ["/gateway"]
