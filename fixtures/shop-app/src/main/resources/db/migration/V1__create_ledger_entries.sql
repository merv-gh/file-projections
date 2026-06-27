-- Flyway migration: creates the ledger_entries table written by RealPaymentService.
CREATE TABLE ledger_entries (
    id         BIGSERIAL PRIMARY KEY,
    order_id   VARCHAR(64) NOT NULL,
    amount     NUMERIC(12,2) NOT NULL,
    status     VARCHAR(16) NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT now()
);

CREATE INDEX idx_ledger_entries_order_id ON ledger_entries (order_id);
