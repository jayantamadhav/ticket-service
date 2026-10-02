BEGIN;

ALTER TABLE reservations DROP CONSTRAINT reservations_status_check;
ALTER TABLE reservations ADD CONSTRAINT reservations_status_check
    CHECK (status IN ('held', 'confirmed', 'cancelled'));
ALTER TABLE reservations ALTER COLUMN status SET DEFAULT 'held';

COMMIT;
