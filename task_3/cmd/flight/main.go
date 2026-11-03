package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"flightbooking/internal/flight"
	"flightbooking/internal/pb"
	"flightbooking/internal/storage"
	"google.golang.org/grpc"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	db, err := storage.Open(ctx, "flight")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	service := flight.New(db)
	defer service.Close()
	server := grpc.NewServer(grpc.UnaryInterceptor(flight.Auth(os.Getenv("GRPC_API_KEY"))))
	pb.RegisterFlightServiceServer(server, service)
	listener, err := net.Listen("tcp", storage.Env("LISTEN_ADDR", ":50051"))
	if err != nil {
		log.Fatal(err)
	}
	go func() { <-ctx.Done(); server.Stop() }()
	log.Print("Flight gRPC listening on ", listener.Addr())
	if err = server.Serve(listener); err != nil {
		log.Print(err)
	}
}
