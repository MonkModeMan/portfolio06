FROM golang:1.22-alpine AS build

WORKDIR /app
COPY go.mod ./
RUN go mod download

COPY . .
RUN go mod tidy && go build -o /bin/portfolio06 ./cmd/server

FROM alpine:3.20

WORKDIR /app
RUN apk add --no-cache ca-certificates
COPY --from=build /bin/portfolio06 /app/portfolio06
COPY web /app/web

ENV PORT=8080
EXPOSE 8080

CMD ["/app/portfolio06"]
