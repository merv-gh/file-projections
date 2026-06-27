package com.acme.shop;

import javax.persistence.Entity;
import javax.persistence.Id;
import javax.persistence.Table;
import java.math.BigDecimal;

/**
 * JPA entity persisted to the "ledger_entries" table. @Table makes the physical
 * table name explicit (the join key shared with the Flyway migration and
 * postgres-watch).
 */
@Entity
@Table(name = "ledger_entries")
public class LedgerEntry {
    @Id
    private Long id;
    private String orderId;
    private BigDecimal amount;
    private String status;

    public String getOrderId() { return orderId; }
    public BigDecimal getAmount() { return amount; }
}
