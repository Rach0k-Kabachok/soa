CREATE TABLE airlines (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL UNIQUE CHECK (length(trim(name)) > 0)
);
CREATE TABLE flights (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    flight_number text NOT NULL CHECK (length(trim(flight_number)) > 0),
    airline_id bigint NOT NULL REFERENCES airlines(id),
    origin char(3) NOT NULL CHECK (origin ~ '^[A-Z]{3}$'),
    destination char(3) NOT NULL CHECK (destination ~ '^[A-Z]{3}$'),
    departure_time timestamptz NOT NULL,
    arrival_time timestamptz NOT NULL CHECK (arrival_time > departure_time),
    total_seats integer NOT NULL CHECK (total_seats > 0),
    available_seats integer NOT NULL CHECK (available_seats BETWEEN 0 AND total_seats),
    price bigint NOT NULL CHECK (price > 0),
    status text NOT NULL CHECK (status IN ('SCHEDULED','DEPARTED','CANCELLED','COMPLETED')),
    CHECK (origin <> destination)
);
CREATE UNIQUE INDEX flight_number_departure_date
    ON flights (flight_number, ((departure_time AT TIME ZONE 'UTC')::date));
CREATE INDEX flight_search ON flights(origin, destination, departure_time) WHERE status='SCHEDULED';
CREATE TABLE seat_reservations (
    booking_id uuid PRIMARY KEY,
    flight_id bigint NOT NULL REFERENCES flights(id),
    seat_count integer NOT NULL CHECK (seat_count > 0),
    status text NOT NULL CHECK (status IN ('ACTIVE','RELEASED','EXPIRED'))
);
-- A release can arrive before a timed-out reserve. This durable fence prevents
-- the late reserve from consuming seats after compensation has already finished.
CREATE TABLE reservation_cancellations (booking_id uuid PRIMARY KEY);
