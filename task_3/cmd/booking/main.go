package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"flightbooking/internal/booking"
	"flightbooking/internal/pb"
	"flightbooking/internal/rpcclient"
	"flightbooking/internal/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	db, err := storage.Open(ctx, "booking")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	conn, err := grpc.NewClient(storage.Env("FLIGHT_ADDR", "flight:50051"), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry(),
		grpc.WithUnaryInterceptor(rpcclient.NewBreaker().Interceptor(os.Getenv("GRPC_API_KEY"), storage.Duration("GRPC_ATTEMPT_TIMEOUT", "1s"))))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	service := booking.New(db, pb.NewFlightServiceClient(conn))
	go service.Recover(ctx)
	server := &http.Server{Addr: storage.Env("LISTEN_ADDR", ":8080"), Handler: service.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: time.Minute}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	log.Print("Booking REST listening on ", server.Addr)
	if err = server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
