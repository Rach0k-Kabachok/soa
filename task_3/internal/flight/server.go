package flight

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"flightbooking/internal/pb"
	"flightbooking/internal/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Server struct {
	pb.UnimplementedFlightServiceServer
	db    *pgxpool.Pool
	cache *cache
	mu    sync.RWMutex
}

func New(db *pgxpool.Pool) *Server { return &Server{db: db, cache: newCache()} }
func (s *Server) Close()           { s.cache.client.Close() }

func Auth(key string) grpc.UnaryServerInterceptor {
	if key == "" {
		panic("GRPC_API_KEY is required")
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		values := md.Get("x-api-key")
		if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte(key)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "invalid API key")
		}
		result, err := handler(ctx, req)
		return result, rpcError(err)
	}
}

func rpcError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "request deadline exceeded")
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "request cancelled")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.NotFound, "flight not found")
	}
	slog.Error("flight database error", "error", err)
	return status.Error(codes.Internal, "database operation failed")
}

const flightColumns = `f.id,f.flight_number,a.name,f.origin,f.destination,f.departure_time,f.arrival_time,f.total_seats,f.available_seats,f.price,f.status`
const flightFrom = ` FROM flights f JOIN airlines a ON a.id=f.airline_id `

func scanFlight(row interface{ Scan(...any) error }) (*pb.Flight, error) {
	f := new(pb.Flight)
	var departure, arrival time.Time
	var state string
	err := row.Scan(&f.Id, &f.FlightNumber, &f.Airline, &f.Origin, &f.Destination, &departure, &arrival, &f.TotalSeats, &f.AvailableSeats, &f.Price, &state)
	f.DepartureTime = timestamppb.New(departure)
	f.ArrivalTime = timestamppb.New(arrival)
	f.Status = pb.FlightStatus(pb.FlightStatus_value[state])
	return f, err
}

var iata = regexp.MustCompile(`^[A-Z]{3}$`)

func (s *Server) SearchFlights(ctx context.Context, r *pb.SearchFlightsRequest) (*pb.SearchFlightsResponse, error) {
	if !iata.MatchString(r.Origin) || !iata.MatchString(r.Destination) || r.Origin == r.Destination {
		return nil, status.Error(codes.InvalidArgument, "distinct origin and destination IATA codes are required")
	}
	date := ""
	if r.Date != nil {
		if r.Date.CheckValid() != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid date")
		}
		d := r.Date.AsTime()
		if !d.Equal(d.Truncate(24 * time.Hour)) {
			return nil, status.Error(codes.InvalidArgument, "date must be midnight UTC")
		}
		date = d.Format("2006-01-02")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := fmt.Sprintf("search:%s:%s:%s", r.Origin, r.Destination, date)
	out := &pb.SearchFlightsResponse{}
	if s.cache.get(ctx, key, out) {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT `+flightColumns+flightFrom+`WHERE f.origin=$1 AND f.destination=$2 AND f.status='SCHEDULED'
	 AND ($3::date IS NULL OR (f.departure_time >= $3::date::timestamp AT TIME ZONE 'UTC' AND f.departure_time < ($3::date+1)::timestamp AT TIME ZONE 'UTC')) ORDER BY f.departure_time,f.id`, r.Origin, r.Destination, nullableDate(date))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		f, err := scanFlight(rows)
		if err != nil {
			return nil, err
		}
		out.Flights = append(out.Flights, f)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	s.cache.set(ctx, key, out)
	return out, nil
}

func nullableDate(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Server) GetFlight(ctx context.Context, r *pb.GetFlightRequest) (*pb.Flight, error) {
	if r.Id <= 0 {
		return nil, status.Error(codes.InvalidArgument, "id must be positive")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := fmt.Sprintf("flight:%d", r.Id)
	out := new(pb.Flight)
	if s.cache.get(ctx, key, out) {
		return out, nil
	}
	out, err := scanFlight(s.db.QueryRow(ctx, "SELECT "+flightColumns+flightFrom+"WHERE f.id=$1", r.Id))
	if err != nil {
		return nil, err
	}
	s.cache.set(ctx, key, out)
	return out, nil
}

func (s *Server) ReserveSeats(ctx context.Context, r *pb.ReserveSeatsRequest) (*pb.SeatReservation, error) {
	id, err := uuid.Parse(r.BookingId)
	if err != nil || r.FlightId <= 0 || r.SeatCount <= 0 {
		return nil, status.Error(codes.InvalidArgument, "valid booking UUID, flight_id and positive seat_count required")
	}
	r.BookingId = id.String()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Even an ambiguous commit error must invalidate potentially changed data.
	defer s.cache.invalidate()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if err = storage.Lock(ctx, tx, r.BookingId); err != nil {
		return nil, err
	}
	var cancelled bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM reservation_cancellations WHERE booking_id=$1)", r.BookingId).Scan(&cancelled); err != nil {
		return nil, err
	}
	if cancelled {
		return nil, status.Error(codes.FailedPrecondition, "reservation already released")
	}
	out := &pb.SeatReservation{BookingId: r.BookingId}
	var state string
	err = tx.QueryRow(ctx, "SELECT flight_id,seat_count,status FROM seat_reservations WHERE booking_id=$1 FOR UPDATE", r.BookingId).Scan(&out.FlightId, &out.SeatCount, &state)
	if err == nil {
		if out.FlightId != r.FlightId || out.SeatCount != r.SeatCount {
			return nil, status.Error(codes.AlreadyExists, "booking_id has different reservation parameters")
		}
		if state != "ACTIVE" {
			return nil, status.Error(codes.FailedPrecondition, "reservation is not active")
		}
		out.Status = pb.ReservationStatus_ACTIVE
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var available int32
	if err = tx.QueryRow(ctx, "SELECT available_seats,status FROM flights WHERE id=$1 FOR UPDATE", r.FlightId).Scan(&available, &state); err != nil {
		return nil, err
	}
	if state != "SCHEDULED" {
		return nil, status.Error(codes.FailedPrecondition, "flight is not scheduled")
	}
	if available < r.SeatCount {
		return nil, status.Error(codes.ResourceExhausted, "not enough seats")
	}
	if _, err = tx.Exec(ctx, "UPDATE flights SET available_seats=available_seats-$1 WHERE id=$2", r.SeatCount, r.FlightId); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO seat_reservations VALUES($1,$2,$3,'ACTIVE')", r.BookingId, r.FlightId, r.SeatCount); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &pb.SeatReservation{BookingId: r.BookingId, FlightId: r.FlightId, SeatCount: r.SeatCount, Status: pb.ReservationStatus_ACTIVE}, nil
}

func (s *Server) ReleaseReservation(ctx context.Context, r *pb.ReleaseReservationRequest) (*emptypb.Empty, error) {
	id, err := uuid.Parse(r.BookingId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "valid booking UUID required")
	}
	r.BookingId = id.String()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Even an ambiguous commit error must invalidate potentially changed data.
	defer s.cache.invalidate()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if err = storage.Lock(ctx, tx, r.BookingId); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO reservation_cancellations VALUES($1) ON CONFLICT DO NOTHING", r.BookingId); err != nil {
		return nil, err
	}
	var flightID int64
	var count int32
	var state string
	err = tx.QueryRow(ctx, "SELECT flight_id,seat_count,status FROM seat_reservations WHERE booking_id=$1 FOR UPDATE", r.BookingId).Scan(&flightID, &count, &state)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err == nil && state == "ACTIVE" {
		if _, err = tx.Exec(ctx, "UPDATE flights SET available_seats=available_seats+$1 WHERE id=$2", count, flightID); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, "UPDATE seat_reservations SET status='RELEASED' WHERE booking_id=$1", r.BookingId); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) UpdateFlight(ctx context.Context, r *pb.UpdateFlightRequest) (*pb.Flight, error) {
	if r.Id <= 0 || r.Price <= 0 || r.Status < pb.FlightStatus_SCHEDULED || r.Status > pb.FlightStatus_COMPLETED {
		return nil, status.Error(codes.InvalidArgument, "positive id, price and valid status required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Even an ambiguous commit error must invalidate potentially changed data.
	defer s.cache.invalidate()
	out, err := scanFlight(s.db.QueryRow(ctx, `WITH changed AS (UPDATE flights SET price=$1,status=$2 WHERE id=$3 RETURNING *) SELECT `+flightColumns+` FROM changed f JOIN airlines a ON a.id=f.airline_id`, r.Price, r.Status.String(), r.Id))
	if err != nil {
		return nil, err
	}
	return out, nil
}
