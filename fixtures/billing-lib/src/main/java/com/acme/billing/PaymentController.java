package com.acme.billing;

import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RestController;

import java.math.BigDecimal;

/**
 * Spring entrypoint — lives IN THE LIBRARY. This is the tricky part: the
 * controller is here, but the only PaymentService it knows is the abstract one.
 * The real work happens in the app repo's override. A clear path is invisible
 * from this repo alone.
 */
@RestController
public class PaymentController {

    // Field typed as the ABSTRACTION. Spring injects the app's concrete bean.
    private final AbstractPaymentService paymentService;

    public PaymentController(AbstractPaymentService paymentService) {
        this.paymentService = paymentService;
    }

    @PostMapping("/charge")
    public Receipt charge(@RequestBody Order order) {
        if (order.isExpress()) {
            // express path
            return paymentService.process(order);
        }
        // standard path
        return paymentService.pay(order);
    }
}
