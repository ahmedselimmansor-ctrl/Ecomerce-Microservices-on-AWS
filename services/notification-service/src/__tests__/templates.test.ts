import { describe, expect, it } from 'vitest';

import { render, templateExists } from '../templates.js';

/**
 * The template renderer.
 *
 * Two of these assertions are about security rather than presentation, and
 * they are the reason this file exists: an email body is assembled from
 * customer-supplied values — a product title, a display name — and one of the
 * two outputs is HTML.
 */
describe('render', () => {
  it('substitutes into subject, text and html', () => {
    const out = render('order_confirmed', 'en-GB', {
      name: 'Ahmed',
      orderRef: 'ord_01H',
      total: 'EGP 1,299.00',
      orderUrl: 'https://souq.dev/orders/ord_01H',
    });

    expect(out.subject).toContain('ord_01H');
    expect(out.text).toContain('Ahmed');
    expect(out.html).toContain('Ahmed');
  });

  /**
   * The plain-text body must NOT be HTML-escaped.
   *
   * Mustache escapes by default, which turns an apostrophe in a product title
   * into `&#39;` — correct for HTML, wrong for a plain-text email, where the
   * customer sees the entity as literal characters.
   */
  it('leaves the text body unescaped', () => {
    const out = render('back_in_stock', 'en-GB', {
      name: "O'Brien",
      productTitle: "Sony's WH-1000XM5 & case",
      productUrl: 'https://souq.dev/p/x',
      unsubscribeUrl: 'https://souq.dev/u',
    });

    expect(out.text).toContain("Sony's WH-1000XM5 & case");
    expect(out.text).not.toContain('&#39;');
    expect(out.text).not.toContain('&amp;');
  });

  /**
   * The HTML body MUST be escaped.
   *
   * A product title is admin-supplied and a display name is customer-supplied.
   * Either reaching an HTML email unescaped is stored XSS in whichever webmail
   * client renders it.
   */
  it('escapes html in the html body', () => {
    const out = render('back_in_stock', 'en-GB', {
      name: '<script>alert(1)</script>',
      productTitle: '<img src=x onerror=alert(1)>',
      productUrl: 'https://souq.dev/p/x',
      unsubscribeUrl: 'https://souq.dev/u',
    });

    // The assertion is about executable markup, not about the substring.
    // `onerror=` survives as inert text inside an escaped tag, which is fine —
    // what must not survive is an actual element.
    expect(out.html).not.toContain('<script>');
    expect(out.html).not.toContain('<img');
    expect(out.html).toContain('&lt;script&gt;');
    expect(out.html).toContain('&lt;img');
  });

  /**
   * Falling back per FIELD rather than per template.
   *
   * A half-translated template should still send, in a mix, rather than not
   * send at all — an order confirmation is worth more to the customer in the
   * wrong language than not at all.
   */
  it('falls back to en-GB for a locale it does not have', () => {
    const out = render('order_confirmed', 'fr-FR', {
      name: 'Marie', orderRef: 'ord_1', total: 'EGP 1', orderUrl: 'https://souq.dev/o',
    });

    expect(out.subject.length).toBeGreaterThan(0);
    expect(out.text).toContain('Marie');
  });

  it('matches a bare language against a regional locale', () => {
    const arabic = render('order_confirmed', 'ar', {
      name: 'أحمد', orderRef: 'ord_1', total: 'EGP 1', orderUrl: 'https://souq.dev/o',
    });
    const egyptian = render('order_confirmed', 'ar-EG', {
      name: 'أحمد', orderRef: 'ord_1', total: 'EGP 1', orderUrl: 'https://souq.dev/o',
    });

    expect(arabic.subject).toBe(egyptian.subject);
  });

  /** Arabic must render right-to-left, or the email is unreadable. */
  it('sets dir=rtl for arabic', () => {
    const arabic = render('order_confirmed', 'ar-EG', {
      name: 'أحمد', orderRef: 'ord_1', total: 'EGP 1', orderUrl: 'https://souq.dev/o',
    });
    const english = render('order_confirmed', 'en-GB', {
      name: 'Ahmed', orderRef: 'ord_1', total: 'EGP 1', orderUrl: 'https://souq.dev/o',
    });

    expect(arabic.html).toContain('dir="rtl"');
    expect(english.html).toContain('dir="ltr"');
  });

  /**
   * A missing parameter renders empty rather than the literal `{{name}}`.
   * Mustache's default for an absent key is the empty string, and an email
   * reading "Hello ," is bad; one reading "Hello {{name}}," is worse.
   */
  it('renders a missing parameter as empty, not as the placeholder', () => {
    const out = render('order_confirmed', 'en-GB', { orderRef: 'ord_1' });

    expect(out.text).not.toContain('{{name}}');
    expect(out.text).not.toContain('{{');
  });

  it('throws on an unknown template rather than sending a blank email', () => {
    // @ts-expect-error — deliberately outside the union, which is the point.
    expect(() => render('no_such_template', 'en-GB', {})).toThrow(/unknown template/);
  });
});

describe('templateExists', () => {
  it('recognises every real template', () => {
    for (const name of [
      'order_confirmed', 'order_cancelled', 'order_shipped', 'order_delivered',
      'payment_failed', 'payment_refunded', 'password_reset',
      'email_verification', 'security_alert', 'abandoned_cart', 'back_in_stock',
    ]) {
      expect(templateExists(name), name).toBe(true);
    }
  });

  it('rejects anything else', () => {
    expect(templateExists('order_confirmed; DROP TABLE')).toBe(false);
    expect(templateExists('__proto__')).toBe(false);
    expect(templateExists('')).toBe(false);
  });
});
