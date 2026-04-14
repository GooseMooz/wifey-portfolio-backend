FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/app .

FROM alpine:3.22

WORKDIR /app

RUN adduser -D -u 10001 appuser

COPY --from=build /out/app /app/app

RUN mkdir -p /app/data && chown -R appuser:appuser /app

USER appuser

EXPOSE 3011

CMD ["/app/app"]
