package com.example.shop;

import org.springframework.validation.BindingResult;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RequestParam;
import org.springframework.web.bind.annotation.RestController;

@RestController
@RequestMapping("/orders")
public class OrderController {

	private final OrderRepository orderRepository;

	public OrderController(OrderRepository orderRepository) {
		this.orderRepository = orderRepository;
	}

	@PostMapping("/checkout")
	public String checkout(@RequestBody Order order, @RequestParam String channel, BindingResult result) {
		if (result.hasErrors()) {
			return "validation-error";
		}
		if (order.isExpress()) {
			order.setPriority(10);
			order.setShipping("express");
		} else {
			order.setPriority(1);
			order.setShipping("standard");
		}
		if (channel.equals("web")) {
			order.setSource("web");
		}
		this.orderRepository.save(order);
		return "ok";
	}
}
