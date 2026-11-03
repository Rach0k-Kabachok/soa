FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/flight ./cmd/flight && CGO_ENABLED=0 go build -o /out/booking ./cmd/booking

FROM alpine:3.22
WORKDIR /app
COPY --from=build /out/ /app/
USER 65532:65532
