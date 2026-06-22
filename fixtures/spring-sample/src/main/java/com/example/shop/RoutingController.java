package com.example.shop;

import java.util.List;

import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

// Exercises the constructs the lexical control-flow lens cannot handle:
// an else-if chain, a switch, and a loop. The Joern (CPG) mode should still
// enumerate the distinct paths that reach the save on line 48.
@RestController
@RequestMapping("/routing")
public class RoutingController {

	private final OrderRepository orderRepository;

	public RoutingController(OrderRepository orderRepository) {
		this.orderRepository = orderRepository;
	}

	@PostMapping("/route")
	public String route(@RequestBody Order order, int kind, String region, List<String> tags) {
		if (kind == 1) {
			order.setPriority(1);
		} else if (kind == 2) {
			order.setPriority(5);
		} else {
			order.setPriority(9);
		}

		switch (region) {
			case "EU":
				order.setShipping("eu");
				break;
			case "US":
				order.setShipping("us");
				break;
			default:
				order.setShipping("intl");
		}

		for (String tag : tags) {
			order.setSource(tag);
		}

		this.orderRepository.save(order);
		return "routed";
	}
}
