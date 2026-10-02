BEGIN;

ALTER TABLE reservations DROP CONSTRAINT reservations_idempotency_key_key;
ALTER TABLE reservations ADD CONSTRAINT reservations_user_idempotency_key_unique
    UNIQUE (user_id, idempotency_key);

COMMIT;
