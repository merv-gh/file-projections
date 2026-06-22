package com.example.shop;

import org.springframework.context.event.EventListener;
import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.kafka.core.KafkaTemplate;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Service;

@Service
public class OrderEventService {

	private final KafkaTemplate<String, String> kafkaTemplate;
	private final OrderRepository orderRepository;

	public OrderEventService(KafkaTemplate<String, String> kafkaTemplate, OrderRepository orderRepository) {
		this.kafkaTemplate = kafkaTemplate;
		this.orderRepository = orderRepository;
	}

	@KafkaListener(topics = "orders.incoming")
	public void onIncoming(String message) {
		Order order = Order.parse(message);
		this.orderRepository.save(order);
		this.kafkaTemplate.send("orders.processed", order.getId());
	}

	@Scheduled(fixedRate = 60000)
	public void publishDailySummary() {
		this.kafkaTemplate.send("orders.summary", "daily");
	}

	@EventListener
	public void onApplicationEvent(Object event) {
		this.kafkaTemplate.send("orders.events", event.toString());
	}
}
