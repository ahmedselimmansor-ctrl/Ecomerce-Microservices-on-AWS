package dev.souq.catalog.catalog;

/**
 * An amount in minor units, with its currency.
 *
 * <p>{@code long}, never {@code double} or {@code BigDecimal}-with-scale. This
 * is the same representation used in every other service and in the Zod
 * contracts, and the reason is that 0.1 + 0.2 is not 0.3 in binary floating
 * point. A cart that sums floats is a cart whose total disagrees with the sum
 * of its lines, in a way that appears at the hundredth order rather than the
 * first, so it ships.
 *
 * <p>{@code amount} is in the currency's minor unit — piastres for EGP, cents
 * for USD. There is no scale field: the currency determines it, and carrying a
 * second source of truth for the same fact is how "1000" becomes ten pounds in
 * one service and a thousand in another.
 */
public record Money(long amount, String currency) {

    public Money {
        if (currency == null || currency.length() != 3) {
            throw new IllegalArgumentException("currency must be an ISO 4217 alphabetic code");
        }
        currency = currency.toUpperCase();
    }

    public static Money of(long amount, String currency) {
        return new Money(amount, currency);
    }

    /** Guards arithmetic across currencies, which is always a bug rather than a conversion. */
    private void requireSameCurrency(Money other) {
        if (!currency.equals(other.currency)) {
            throw new IllegalArgumentException(
                    "cannot combine %s and %s".formatted(currency, other.currency));
        }
    }

    public Money plus(Money other) {
        requireSameCurrency(other);
        return new Money(Math.addExact(amount, other.amount), currency);
    }

    public Money minus(Money other) {
        requireSameCurrency(other);
        return new Money(Math.subtractExact(amount, other.amount), currency);
    }

    public boolean isGreaterThan(Money other) {
        requireSameCurrency(other);
        return amount > other.amount;
    }
}
