BEGIN;

CREATE TABLE shows (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    price_paise     BIGINT NOT NULL CHECK (price_paise >= 0),
    per_user_limit  INT NOT NULL DEFAULT 4,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE seats (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    show_id          UUID NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    seat_label       TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'available'
                         CHECK (status IN ('available', 'held', 'confirmed')),
    reservation_id   UUID,
    held_by_user_id  TEXT,
    held_until       TIMESTAMPTZ,
    UNIQUE (show_id, seat_label)
);

CREATE INDEX idx_seats_show_status ON seats (show_id, status);
CREATE INDEX idx_seats_held_until ON seats (held_until) WHERE status = 'held';

CREATE TABLE reservations (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    show_id          UUID NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    user_id          TEXT NOT NULL,
    seats            TEXT[] NOT NULL,
    amount_paise     BIGINT NOT NULL CHECK (amount_paise >= 0),
    status           TEXT NOT NULL DEFAULT 'confirmed'
                         CHECK (status IN ('confirmed', 'cancelled')),
    idempotency_key  TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (idempotency_key)
);

CREATE INDEX idx_reservations_user_show ON reservations (user_id, show_id);

CREATE TABLE user_show_holds (
    show_id      UUID NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    user_id      TEXT NOT NULL,
    held_count   INT NOT NULL DEFAULT 0 CHECK (held_count >= 0),
    PRIMARY KEY (show_id, user_id)
);

COMMIT;
