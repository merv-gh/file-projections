package com.acme.shop;

import com.acme.billing.AbstractPaymentService;
import com.acme.billing.Order;
import com.acme.billing.Receipt;
import org.springframework.stereotype.Service;

/**
 * The concrete override — lives in the APP repo. This is the dependency-inversion
 * resolution target: the library's controller calls AbstractPaymentService.pay(),
 * and at runtime Spring dispatches to THIS method. The line we care about
 * (ledger.write) is only reachable once both repos are loaded and the override is
 * resolved across the boundary.
 */
@Service
public class RealPaymentService extends AbstractPaymentService {

    private final Ledger ledger;

    public RealPaymentService(Ledger ledger) {
        this.ledger = ledger;
    }

    @Override
    public Receipt pay(Order order) {
        Receipt receipt;
        if (order.getAmount().signum() > 0) {
            // the line a reviewer asks about: "how do we end up writing to the ledger?"
            ledger.write(order.getId(), order.getAmount());
            receipt = new Receipt(order.getId(), "PAID");
        } else {
            receipt = new Receipt(order.getId(), "SKIPPED");
        }
        return receipt;
    }
}
