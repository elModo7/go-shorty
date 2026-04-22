FROM golang:1.26.1-alpine AS builder

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o go-shorty .

FROM alpine:3.22

WORKDIR /app

COPY --from=builder /app/go-shorty /app/go-shorty

ENV LISTEN_ADDR=:8080
ENV BASE_URL=http://localhost:8080
ENV DB_PATH=/data/shorty.db

VOLUME ["/data"]
EXPOSE 8080

CMD ["/app/go-shorty"]
