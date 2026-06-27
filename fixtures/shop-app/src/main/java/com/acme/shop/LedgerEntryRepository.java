package com.acme.shop;

import com.acme.billing.Order;
import org.springframework.data.jpa.repository.JpaRepository;

/**
 * Spring Data repository for ledger entries. The entity is the first generic arg
 * (LedgerEntry), which maps to the "ledger_entries" table via @Table. Calling
 * save()/findByOrderId() here is what writes-to / reads-from the table.
 */
public interface LedgerEntryRepository extends JpaRepository<LedgerEntry, Long> {
    LedgerEntry findByOrderId(String orderId);
}
