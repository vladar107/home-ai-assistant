-- A flight booking may hold the flight's number, such as EX123, normalised by the trip package so
-- code can look the flight up. A booking that has none, every one stored before this, keeps an
-- empty number.
ALTER TABLE trip_bookings ADD COLUMN flight_number TEXT NOT NULL DEFAULT ''
    CHECK (length(flight_number) <= 8);
