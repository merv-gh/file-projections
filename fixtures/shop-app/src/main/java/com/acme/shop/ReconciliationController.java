package com.acme.shop;

import com.acme.billing.Order;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RestController;

/**
 * A SECOND entrypoint that also writes to the ledger_entries table — the night-time
 * reconciliation job. This is the bug the demo discovers: unlike RealPaymentService,
 * it persists WITHOUT the `amount > 0` guard, so it writes zero/negative rows the
 * payment path would have skipped. Two writers to one table, one of them wrong.
 */
@RestController
public class ReconciliationController {

    private final LedgerEntryRepository ledgerEntryRepository;

    public ReconciliationController(LedgerEntryRepository ledgerEntryRepository) {
        this.ledgerEntryRepository = ledgerEntryRepository;
    }

    @PostMapping("/reconcile")
    public void reconcile(@RequestBody Order order) {
        LedgerEntry entry = new LedgerEntry();
        // BUG: no `order.getAmount().signum() > 0` check (RealPaymentService has it),
        // so reconciliation writes rows the payment path intentionally skipped.
        ledgerEntryRepository.save(entry);
    }
}
