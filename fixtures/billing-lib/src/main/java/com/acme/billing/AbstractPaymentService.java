package com.acme.billing;

/**
 * The abstraction the library depends on. The library has NO concrete
 * implementation — dependency inversion: the app repo provides one by extending
 * this class. A single-repo view of the library dead-ends here (pay() is abstract).
 */
public abstract class AbstractPaymentService {

    /** Charge the order. The concrete override lives in the app repo. */
    public abstract Receipt pay(Order order);

    /** Template method: shared pre-flight, then the abstract hook. */
    public Receipt process(Order order) {
        validate(order);
        return pay(order);
    }

    protected void validate(Order order) {
        if (order.getAmount() == null) {
            throw new IllegalArgumentException("amount required");
        }
    }
}
