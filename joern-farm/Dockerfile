FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum* ./
RUN [ -f go.sum ] && go mod download || true
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /joern-farm .

FROM alpine:3.19
RUN apk add --no-cache docker-cli ca-certificates
COPY --from=builder /joern-farm /usr/local/bin/joern-farm
WORKDIR /app
EXPOSE 9090
CMD ["joern-farm"]
