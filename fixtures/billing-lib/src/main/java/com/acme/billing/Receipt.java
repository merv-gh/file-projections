package com.acme.billing;

public class Receipt {
    private final String orderId;
    private final String status;

    public Receipt(String orderId, String status) {
        this.orderId = orderId;
        this.status = status;
    }

    public String getOrderId() { return orderId; }
    public String getStatus() { return status; }
}
