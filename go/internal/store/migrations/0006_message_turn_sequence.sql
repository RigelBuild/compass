-- Existing messages represent posts made before agent turn sequences were carried.
ALTER TABLE messages
    ADD COLUMN turn_sequence BIGINT NOT NULL DEFAULT 0 CHECK (turn_sequence >= 0);
