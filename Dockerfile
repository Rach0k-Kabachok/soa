FROM golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -o /catalog-service .

FROM scratch
COPY --from=build /catalog-service /catalog-service
EXPOSE 8080
ENTRYPOINT ["/catalog-service"]
