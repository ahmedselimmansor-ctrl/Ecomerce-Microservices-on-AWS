package dev.souq.catalog.catalog;

import java.text.Normalizer;
import java.util.Locale;
import java.util.Set;

/**
 * Turns a title into a URL slug.
 *
 * <p>Three things here are not obvious.
 *
 * <p><b>Unicode is decomposed before stripping.</b> "Café" normalises to
 * "cafe" rather than "caf", because NFD splits "é" into "e" plus a combining
 * accent and only the accent is removed. Stripping non-ASCII from the composed
 * form deletes the whole character, and a catalogue with any European or
 * transliterated Arabic product name ends up full of slugs with holes in them.
 *
 * <p><b>Arabic text produces no ASCII at all.</b> Rather than emit an empty
 * slug — which would collide with every other Arabic title — the caller is
 * told, and falls back to the product id. A slug is a convenience; a unique
 * URL is not.
 *
 * <p><b>A short list of reserved words is rejected.</b> A product legitimately
 * titled "New" would otherwise take {@code /products/new}, which is the admin
 * create route.
 */
public final class Slugs {

    private static final int MAX_LENGTH = 80;

    private static final Set<String> RESERVED = Set.of(
            "new", "edit", "delete", "admin", "api", "search", "cart", "checkout",
            "account", "login", "logout", "register", "null", "undefined");

    private Slugs() {}

    /** Returns the slug, or empty when the input yields nothing usable. */
    public static java.util.Optional<String> from(String input) {
        if (input == null || input.isBlank()) {
            return java.util.Optional.empty();
        }

        String decomposed = Normalizer.normalize(input, Normalizer.Form.NFD);

        StringBuilder out = new StringBuilder(decomposed.length());
        boolean lastWasHyphen = true;   // true, so a leading separator is dropped

        for (int i = 0; i < decomposed.length() && out.length() < MAX_LENGTH; i++) {
            char c = Character.toLowerCase(decomposed.charAt(i));

            if ((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
                out.append(c);
                lastWasHyphen = false;
            } else if (Character.getType(c) == Character.NON_SPACING_MARK) {
                // The accent left over from NFD. Skipped without becoming a
                // separator, so "café" is "cafe" and not "caf-e".
                continue;
            } else if (!lastWasHyphen) {
                out.append('-');
                lastWasHyphen = true;
            }
        }

        while (out.length() > 0 && out.charAt(out.length() - 1) == '-') {
            out.setLength(out.length() - 1);
        }

        String slug = out.toString();
        if (slug.isEmpty() || RESERVED.contains(slug)) {
            return java.util.Optional.empty();
        }
        return java.util.Optional.of(slug);
    }

    /**
     * A slug guaranteed to be unique, given a way to test existence.
     *
     * <p>Appends {@code -2}, {@code -3} … rather than a random suffix, because
     * a slug is user-visible and "wireless-headphones-2" reads as a second
     * product where "wireless-headphones-x7f2a" reads as a bug.
     *
     * <p>The loop is bounded. Past the bound the caller uses the id: this races
     * with a concurrent insert anyway, and the unique index is the real
     * guarantee — this only avoids the common case of a pointless conflict.
     */
    public static String uniqueFrom(String input, String fallbackId,
                                    java.util.function.Predicate<String> taken) {
        String base = from(input).orElse(null);
        if (base == null) {
            return fallbackId.toLowerCase(Locale.ROOT);
        }

        if (!taken.test(base)) {
            return base;
        }

        for (int suffix = 2; suffix <= 50; suffix++) {
            String candidate = base + "-" + suffix;
            if (!taken.test(candidate)) {
                return candidate;
            }
        }

        return base + "-" + fallbackId.toLowerCase(Locale.ROOT);
    }
}
