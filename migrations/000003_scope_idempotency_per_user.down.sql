BEGIN;

ALTER TABLE reservations DROP CONSTRAINT reservations_user_idempotency_key_unique;
ALTER TABLE reservations ADD CONSTRAINT reservations_idempotency_key_key
    UNIQUE (idempotency_key);

COMMIT;
