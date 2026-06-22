package com.example.shop;

public class Order {

	private String id;
	private boolean express;
	private int priority;
	private String shipping;
	private String source;

	public static Order parse(String message) {
		Order order = new Order();
		order.id = message;
		return order;
	}

	public String getId() {
		return id;
	}

	public boolean isExpress() {
		return express;
	}

	public void setPriority(int priority) {
		this.priority = priority;
	}

	public void setShipping(String shipping) {
		this.shipping = shipping;
	}

	public void setSource(String source) {
		this.source = source;
	}
}
