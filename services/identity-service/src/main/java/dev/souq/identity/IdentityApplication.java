package dev.souq.identity;

import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.scheduling.annotation.EnableScheduling;

/**
 * identity-service.
 *
 * Issues and rotates the tokens every other service verifies. Read
 * {@link dev.souq.identity.token.TokenService} before changing anything here:
 * two decisions in that class carry most of the platform's security posture,
 * and both are the unobvious option.
 */
@SpringBootApplication
@EnableScheduling
public class IdentityApplication {
    public static void main(String[] args) {
        SpringApplication.run(IdentityApplication.class, args);
    }
}
