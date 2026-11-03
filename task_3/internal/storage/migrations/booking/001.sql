CREATE TABLE bookings (
    id uuid PRIMARY KEY,
    user_id text NOT NULL CHECK (length(trim(user_id)) > 0),
    flight_id bigint NOT NULL CHECK (flight_id > 0),
    passenger_name text NOT NULL CHECK (length(trim(passenger_name)) > 0),
    passenger_email text NOT NULL CHECK (length(trim(passenger_email)) > 0),
    seat_count integer NOT NULL CHECK (seat_count > 0),
    total_price bigint NOT NULL CHECK (total_price > 0),
    status text NOT NULL CHECK (status IN ('CONFIRMED','CANCELLED')),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX bookings_user ON bookings(user_id);
-- Durable compensation intent. It is removed in the same local transaction as
-- booking creation/cancellation, and survives process/network/DB failures.
CREATE TABLE pending_releases (booking_id uuid PRIMARY KEY);
