package booking

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"flightbooking/internal/pb"
	"flightbooking/internal/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Server struct {
	db      *pgxpool.Pool
	flights pb.FlightServiceClient
}

type Booking struct {
	ID             string    `json:"id"`
	UserID         string    `json:"user_id"`
	FlightID       int64     `json:"flight_id"`
	PassengerName  string    `json:"passenger_name"`
	PassengerEmail string    `json:"passenger_email"`
	SeatCount      int32     `json:"seat_count"`
	TotalPrice     int64     `json:"total_price"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

const columns = `id,user_id,flight_id,passenger_name,passenger_email,seat_count,total_price,status,created_at`

func scanBooking(row interface{ Scan(...any) error }) (*Booking, error) {
	b := new(Booking)
	err := row.Scan(&b.ID, &b.UserID, &b.FlightID, &b.PassengerName, &b.PassengerEmail, &b.SeatCount, &b.TotalPrice, &b.Status, &b.CreatedAt)
	return b, err
}

func New(db *pgxpool.Pool, flights pb.FlightServiceClient) *Server {
	return &Server{db: db, flights: flights}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /flights", s.search)
	mux.HandleFunc("GET /flights/{id}", s.flight)
	mux.HandleFunc("POST /bookings", s.create)
	mux.HandleFunc("GET /bookings/{id}", s.get)
	mux.HandleFunc("GET /bookings", s.list)
	mux.HandleFunc("POST /bookings/{id}/cancel", s.cancel)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func respond(w http.ResponseWriter, value any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, pgx.ErrNoRows) {
			err = status.Error(codes.NotFound, "booking not found")
		}
		switch status.Code(err) {
		case codes.InvalidArgument:
			code = 400
		case codes.NotFound:
			code = 404
		case codes.AlreadyExists, codes.ResourceExhausted, codes.FailedPrecondition:
			code = 409
		case codes.Unauthenticated:
			code = 401
		case codes.Unavailable, codes.DeadlineExceeded:
			code = 503
		case codes.Canceled:
			code = 408
		}
		message := status.Convert(err).Message()
		if code == 500 {
			slog.Error("booking request failed", "error", err)
			message = "database operation failed"
		}
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": message})
		return
	}
	if message, ok := value.(proto.Message); ok {
		data, marshalErr := (protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}).Marshal(message)
		if marshalErr != nil {
			w.WriteHeader(500)
			return
		}
		w.Write(data)
		return
	}
	json.NewEncoder(w).Encode(value)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := &pb.SearchFlightsRequest{Origin: q.Get("origin"), Destination: q.Get("destination")}
	if date := q.Get("date"); date != "" {
		d, err := time.Parse("2006-01-02", date)
		if err != nil {
			respond(w, nil, status.Error(codes.InvalidArgument, "date must be YYYY-MM-DD"))
			return
		}
		req.Date = timestamppb.New(d)
	}
	result, err := s.flights.SearchFlights(r.Context(), req)
	respond(w, result, err)
}

func (s *Server) flight(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		respond(w, nil, status.Error(codes.InvalidArgument, "invalid flight id"))
		return
	}
	result, err := s.flights.GetFlight(r.Context(), &pb.GetFlightRequest{Id: id})
	respond(w, result, err)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var input struct {
		UserID         string `json:"user_id"`
		FlightID       int64  `json:"flight_id"`
		PassengerName  string `json:"passenger_name"`
		PassengerEmail string `json:"passenger_email"`
		SeatCount      int32  `json:"seat_count"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&input)
	if err == nil {
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			err = errors.New("trailing JSON")
		}
	}
	address, emailErr := mail.ParseAddress(input.PassengerEmail)
	if err != nil || strings.TrimSpace(input.UserID) == "" || strings.TrimSpace(input.PassengerName) == "" || emailErr != nil || address.Address != input.PassengerEmail || input.FlightID <= 0 || input.SeatCount <= 0 {
		respond(w, nil, status.Error(codes.InvalidArgument, "valid user_id, flight_id, passenger_name, passenger_email and positive seat_count required"))
		return
	}
	ctx := r.Context()
	f, err := s.flights.GetFlight(ctx, &pb.GetFlightRequest{Id: input.FlightID})
	if err != nil {
		respond(w, nil, err)
		return
	}
	if f.Price <= 0 || f.Price > math.MaxInt64/int64(input.SeatCount) {
		respond(w, nil, status.Error(codes.InvalidArgument, "total price overflow"))
		return
	}
	b := &Booking{ID: uuid.NewString(), UserID: input.UserID, FlightID: input.FlightID, PassengerName: input.PassengerName, PassengerEmail: input.PassengerEmail, SeatCount: input.SeatCount, TotalPrice: f.Price * int64(input.SeatCount), Status: "CONFIRMED"}
	err = s.createBooking(ctx, b)
	if err != nil {
		s.compensate(b.ID)
	}
	respond(w, b, err)
}

func (s *Server) createBooking(ctx context.Context, b *Booking) error {
	conn, unlock, err := storage.AcquireLock(ctx, s.db, b.ID)
	if err != nil {
		return err
	}
	defer unlock()
	// Persist the intent independently, before the remote operation. The advisory
	// lock keeps the recovery worker away until this transaction has finished.
	if _, err = conn.Exec(ctx, "INSERT INTO pending_releases VALUES($1)", b.ID); err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = s.flights.ReserveSeats(ctx, &pb.ReserveSeatsRequest{BookingId: b.ID, FlightId: b.FlightID, SeatCount: b.SeatCount}); err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `INSERT INTO bookings(id,user_id,flight_id,passenger_name,passenger_email,seat_count,total_price,status)
	 VALUES($1,$2,$3,$4,$5,$6,$7,'CONFIRMED') RETURNING created_at`, b.ID, b.UserID, b.FlightID, b.PassengerName, b.PassengerEmail, b.SeatCount, b.TotalPrice).Scan(&b.CreatedAt)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM pending_releases WHERE booking_id=$1", b.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func bookingID(r *http.Request) (string, error) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return "", status.Error(codes.InvalidArgument, "valid booking UUID required")
	}
	return id.String(), nil
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	id, err := bookingID(r)
	if err != nil {
		respond(w, nil, err)
		return
	}
	b, err := scanBooking(s.db.QueryRow(r.Context(), "SELECT "+columns+" FROM bookings WHERE id=$1", id))
	respond(w, b, err)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	user := r.URL.Query().Get("user_id")
	if strings.TrimSpace(user) == "" {
		respond(w, nil, status.Error(codes.InvalidArgument, "user_id is required"))
		return
	}
	rows, err := s.db.Query(r.Context(), "SELECT "+columns+" FROM bookings WHERE user_id=$1 ORDER BY created_at,id", user)
	if err != nil {
		respond(w, nil, err)
		return
	}
	defer rows.Close()
	out := []*Booking{}
	for rows.Next() {
		b, err := scanBooking(rows)
		if err != nil {
			respond(w, nil, err)
			return
		}
		out = append(out, b)
	}
	respond(w, out, rows.Err())
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	id, err := bookingID(r)
	if err != nil {
		respond(w, nil, err)
		return
	}
	b, err := s.cancelBooking(r.Context(), id)
	respond(w, b, err)
}

func (s *Server) cancelBooking(ctx context.Context, id string) (*Booking, error) {
	conn, unlock, err := storage.AcquireLock(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	b, err := scanBooking(conn.QueryRow(ctx, "SELECT "+columns+" FROM bookings WHERE id=$1", id))
	if err != nil {
		return nil, err
	}
	if b.Status != "CONFIRMED" {
		return nil, status.Error(codes.FailedPrecondition, "booking is not CONFIRMED")
	}
	if _, err = conn.Exec(ctx, "INSERT INTO pending_releases VALUES($1) ON CONFLICT DO NOTHING", id); err != nil {
		return nil, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if err = s.release(ctx, tx, id); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	b.Status = "CANCELLED"
	return b, nil
}

func (s *Server) release(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := s.flights.ReleaseReservation(ctx, &pb.ReleaseReservationRequest{BookingId: id}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE bookings SET status='CANCELLED' WHERE id=$1", id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "DELETE FROM pending_releases WHERE booking_id=$1", id)
	return err
}

func (s *Server) compensate(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.recoverOne(ctx, id); err != nil {
		slog.Warn("compensation pending", "booking_id", id, "error", err)
	}
}

func (s *Server) recoverOne(ctx context.Context, id string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var locked bool
	if err = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))", id).Scan(&locked); err != nil || !locked {
		return err
	}
	var pending bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pending_releases WHERE booking_id=$1)", id).Scan(&pending); err != nil || !pending {
		return err
	}
	if err = s.release(ctx, tx, id); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err == nil {
		slog.Info("reservation compensation completed", "booking_id", id)
	}
	return err
}

func (s *Server) Recover(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		rows, err := s.db.Query(ctx, "SELECT booking_id::text FROM pending_releases LIMIT 100")
		if err != nil {
			slog.Warn("compensation scan failed", "error", err)
			continue
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			s.compensate(id)
		}
	}
}
