FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod ./
COPY main.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -o cf-failover .

FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata

COPY --from=builder /app/cf-failover /usr/local/bin/cf-failover

USER nobody:nobody

ENTRYPOINT ["cf-failover"]
