FROM golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go generate ./internal/api && CGO_ENABLED=0 go build -o /catalog-service .

FROM alpine:3.22
COPY --from=build /catalog-service /catalog-service
EXPOSE 8080
ENTRYPOINT ["/catalog-service"]
