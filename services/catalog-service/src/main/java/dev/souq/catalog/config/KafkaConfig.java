package dev.souq.catalog.config;

import java.util.HashMap;
import java.util.Map;

import org.apache.kafka.clients.consumer.ConsumerConfig;
import org.apache.kafka.common.serialization.StringDeserializer;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;
import org.springframework.kafka.config.ConcurrentKafkaListenerContainerFactory;
import org.springframework.kafka.core.ConsumerFactory;
import org.springframework.kafka.core.DefaultKafkaConsumerFactory;
import org.springframework.kafka.listener.ContainerProperties;

/**
 * The consumer side.
 *
 * <p>Three settings here decide whether this consumer degrades gracefully or
 * takes the service down with it.
 *
 * <p><b>Manual acknowledgement.</b> Auto-commit acknowledges on a timer,
 * independent of whether the handler succeeded, so a crash mid-batch loses
 * every event the timer had already committed. With {@code MANUAL_IMMEDIATE}
 * the offset moves only after the handler says so.
 *
 * <p><b>{@code auto.offset.reset = latest}.</b> A new consumer group joining a
 * compacted topic with {@code earliest} replays the entire catalogue. That is
 * occasionally what you want — a rebuild — but as the default it means every
 * accidental group-id typo triggers a full replay in production.
 *
 * <p><b>A bounded {@code max.poll.records}.</b> The default of 500 with a
 * five-minute {@code max.poll.interval.ms} means a slow batch silently exceeds
 * the interval, the broker decides the consumer is dead, and the resulting
 * rebalance reprocesses everything — which then takes just as long, forever.
 */
@Configuration
public class KafkaConfig {

    @Bean
    public ConsumerFactory<String, String> consumerFactory(
            @Value("${spring.kafka.bootstrap-servers}") String brokers) {

        Map<String, Object> props = new HashMap<>();
        props.put(ConsumerConfig.BOOTSTRAP_SERVERS_CONFIG, brokers);
        props.put(ConsumerConfig.KEY_DESERIALIZER_CLASS_CONFIG, StringDeserializer.class);
        props.put(ConsumerConfig.VALUE_DESERIALIZER_CLASS_CONFIG, StringDeserializer.class);
        props.put(ConsumerConfig.ENABLE_AUTO_COMMIT_CONFIG, false);
        props.put(ConsumerConfig.AUTO_OFFSET_RESET_CONFIG, "latest");
        props.put(ConsumerConfig.MAX_POLL_RECORDS_CONFIG, 50);
        // Read committed only. Without this the consumer can see writes from a
        // producer transaction that later aborts.
        props.put(ConsumerConfig.ISOLATION_LEVEL_CONFIG, "read_committed");

        return new DefaultKafkaConsumerFactory<>(props);
    }

    @Bean
    public ConcurrentKafkaListenerContainerFactory<String, String> kafkaListenerContainerFactory(
            ConsumerFactory<String, String> consumerFactory) {

        var factory = new ConcurrentKafkaListenerContainerFactory<String, String>();
        factory.setConsumerFactory(consumerFactory);
        factory.getContainerProperties().setAckMode(ContainerProperties.AckMode.MANUAL_IMMEDIATE);

        // One thread per partition, capped. Concurrency above the partition
        // count buys nothing — the extra consumers sit idle — and each one still
        // holds a database connection from a pool sized for the web tier.
        factory.setConcurrency(3);

        return factory;
    }
}
