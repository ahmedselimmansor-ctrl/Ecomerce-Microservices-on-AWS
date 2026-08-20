package dev.souq.catalog.catalog;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

class MoneyTest {

    @Test
    @DisplayName("normalises the currency code")
    void normalisesCurrency() {
        assertEquals("EGP", Money.of(100, "egp").currency());
    }

    @Test
    @DisplayName("rejects anything that is not a three-letter code")
    void rejectsBadCurrency() {
        assertThrows(IllegalArgumentException.class, () -> Money.of(100, "EGPX"));
        assertThrows(IllegalArgumentException.class, () -> Money.of(100, "EG"));
        assertThrows(IllegalArgumentException.class, () -> Money.of(100, null));
    }

    /**
     * The reason amounts are integers.
     *
     * <p>Summing 0.1 three times in binary floating point gives
     * 0.30000000000000004. Over a cart of twenty lines the error is invisible
     * on screen and real in the total, which is how a checkout ends up
     * disagreeing with the sum of its own lines.
     */
    @Test
    @DisplayName("sums exactly, where floating point would not")
    void sumsExactly() {
        var total = Money.of(0, "EGP");
        for (int i = 0; i < 1_000; i++) {
            total = total.plus(Money.of(10, "EGP"));
        }
        assertEquals(10_000, total.amount());
    }

    @Test
    @DisplayName("refuses to combine different currencies")
    void refusesMixedCurrencies() {
        var egp = Money.of(100, "EGP");
        var usd = Money.of(100, "USD");

        assertThrows(IllegalArgumentException.class, () -> egp.plus(usd));
        assertThrows(IllegalArgumentException.class, () -> egp.minus(usd));
        assertThrows(IllegalArgumentException.class, () -> egp.isGreaterThan(usd));
    }

    /**
     * Overflow throws rather than wrapping. A wrapped total is a negative
     * charge, and a negative charge is a refund nobody authorised.
     */
    @Test
    @DisplayName("throws on overflow instead of wrapping to a negative total")
    void overflowThrows() {
        var huge = Money.of(Long.MAX_VALUE, "EGP");
        assertThrows(ArithmeticException.class, () -> huge.plus(Money.of(1, "EGP")));
    }

    @Test
    @DisplayName("compares within a currency")
    void compares() {
        assertTrue(Money.of(200, "EGP").isGreaterThan(Money.of(100, "EGP")));
        assertEquals(100, Money.of(200, "EGP").minus(Money.of(100, "EGP")).amount());
    }
}
