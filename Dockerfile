FROM golang:1.24-alpine AS builder
WORKDIR /src/app
COPY go.mod go.sum main.go ./
RUN go build -o printbot

FROM alpine
WORKDIR /app/
COPY --from=builder /src/app/printbot ./printbot
CMD ["/app/printbot"]
