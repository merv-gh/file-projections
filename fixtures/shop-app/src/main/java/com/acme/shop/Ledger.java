package com.acme.shop;

import java.math.BigDecimal;

/**
 * The sink the trace target writes to. Performs the IO side-effect (db write).
 */
public class Ledger {

    public void write(String orderId, BigDecimal amount) {
        // simulated persistence — the side-effect the path leads to
        System.out.println("LEDGER " + orderId + " " + amount);
    }
}
