package com.acme.shop;

import java.math.BigDecimal;

/**
 * The sink the trace target writes to. Persists through the JPA repository, so the
 * write lands on the "ledger_entries" table — the node the table-trace terminates at.
 */
public class Ledger {

    private final LedgerEntryRepository ledgerEntryRepository;

    public Ledger(LedgerEntryRepository ledgerEntryRepository) {
        this.ledgerEntryRepository = ledgerEntryRepository;
    }

    public void write(String orderId, BigDecimal amount) {
        LedgerEntry entry = new LedgerEntry();
        // the side-effect the path leads to: a write to the ledger_entries table
        ledgerEntryRepository.save(entry);
    }
}
