package com.acme.billing;

import java.math.BigDecimal;

/**
 * An order to be charged. Lives in the library so both the controller (here) and
 * the concrete service (in the app repo) share the type.
 */
public class Order {
    private final String id;
    private final BigDecimal amount;
    private final boolean express;

    public Order(String id, BigDecimal amount, boolean express) {
        this.id = id;
        this.amount = amount;
        this.express = express;
    }

    public String getId() { return id; }
    public BigDecimal getAmount() { return amount; }
    public boolean isExpress() { return express; }
}
