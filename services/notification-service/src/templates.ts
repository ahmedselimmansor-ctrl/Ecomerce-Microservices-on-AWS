import Mustache from 'mustache';

/**
 * Message templates.
 *
 * In-process rather than a service call, for one reason: notification is
 * downstream of everything, and it must be able to tell a customer their
 * payment failed even when the rest of the platform is having the bad day that
 * caused it.
 *
 * Every template has an explicit text/plain body. It is not a fallback — it is
 * what a screen reader, a smartwatch and a spam filter all actually read, and
 * an HTML-only email measurably lands in spam more often.
 */

export type TemplateName =
  | 'order_confirmed' | 'order_cancelled' | 'order_shipped' | 'order_delivered'
  | 'payment_failed' | 'payment_refunded'
  | 'password_reset' | 'email_verification' | 'security_alert'
  | 'abandoned_cart' | 'back_in_stock';

interface Template {
  subject: Record<string, string>;
  text: Record<string, string>;
  html?: Record<string, string>;
}

// en-GB and ar-EG. Arabic is not an afterthought here: it is the primary
// language of the primary market, and the HTML wrapper sets dir="rtl" for it.
const TEMPLATES: Record<TemplateName, Template> = {
  order_confirmed: {
    subject: {
      'en-GB': 'Order {{orderRef}} confirmed',
      'ar-EG': 'تم تأكيد طلبك {{orderRef}}',
    },
    text: {
      'en-GB': 'Hello {{name}},\n\nYour order {{orderRef}} is confirmed.\n\nTotal: {{total}}\nDelivery: {{estimatedDelivery}}\n\nTrack it any time: {{orderUrl}}\n\nSOUQ',
      'ar-EG': 'أهلاً {{name}}،\n\nتم تأكيد طلبك {{orderRef}}.\n\nالإجمالي: {{total}}\nالتسليم المتوقع: {{estimatedDelivery}}\n\nتابع طلبك: {{orderUrl}}\n\nسوق',
    },
  },
  order_cancelled: {
    subject: {
      'en-GB': 'Order {{orderRef}} could not be completed',
      'ar-EG': 'تعذّر إتمام طلبك {{orderRef}}',
    },
    text: {
      // Says plainly that no money was taken. The single most common support
      // contact after a failed checkout is "have I been charged?".
      'en-GB': 'Hello {{name}},\n\nWe could not complete order {{orderRef}}.\n\nReason: {{reason}}\n\nYou have not been charged.\n\nSOUQ',
      'ar-EG': 'أهلاً {{name}}،\n\nلم نتمكن من إتمام طلبك {{orderRef}}.\n\nالسبب: {{reason}}\n\nلم يتم خصم أي مبلغ.\n\nسوق',
    },
  },
  order_shipped: {
    subject: { 'en-GB': 'Order {{orderRef}} is on its way', 'ar-EG': 'طلبك {{orderRef}} في الطريق' },
    text: {
      'en-GB': 'Hello {{name}},\n\nOrder {{orderRef}} has shipped with {{carrier}}.\n\nTracking: {{trackingNumber}}\n{{trackingUrl}}\n\nSOUQ',
      'ar-EG': 'أهلاً {{name}}،\n\nتم شحن طلبك {{orderRef}} عبر {{carrier}}.\n\nرقم التتبع: {{trackingNumber}}\n{{trackingUrl}}\n\nسوق',
    },
  },
  order_delivered: {
    subject: { 'en-GB': 'Order {{orderRef}} delivered', 'ar-EG': 'تم تسليم طلبك {{orderRef}}' },
    text: {
      'en-GB': 'Hello {{name}},\n\nOrder {{orderRef}} was delivered.\n\nTell us how it went: {{reviewUrl}}\n\nSOUQ',
      'ar-EG': 'أهلاً {{name}}،\n\nتم تسليم طلبك {{orderRef}}.\n\nشاركنا رأيك: {{reviewUrl}}\n\nسوق',
    },
  },
  payment_failed: {
    subject: { 'en-GB': 'Payment for {{orderRef}} was declined', 'ar-EG': 'تم رفض الدفع للطلب {{orderRef}}' },
    text: {
      'en-GB': 'Hello {{name}},\n\nYour bank declined the payment for order {{orderRef}}.\n\nNothing has been charged. You can try another card here: {{retryUrl}}\n\nSOUQ',
      'ar-EG': 'أهلاً {{name}}،\n\nرفض البنك عملية الدفع للطلب {{orderRef}}.\n\nلم يتم خصم أي مبلغ. يمكنك المحاولة ببطاقة أخرى: {{retryUrl}}\n\nسوق',
    },
  },
  payment_refunded: {
    subject: { 'en-GB': 'Refund issued for {{orderRef}}', 'ar-EG': 'تم استرداد مبلغ الطلب {{orderRef}}' },
    text: {
      'en-GB': 'Hello {{name}},\n\nWe have refunded {{amount}} for order {{orderRef}}.\n\nIt usually reaches your account in 5-10 working days.\n\nSOUQ',
      'ar-EG': 'أهلاً {{name}}،\n\nتم استرداد {{amount}} للطلب {{orderRef}}.\n\nعادةً يصل خلال 5-10 أيام عمل.\n\nسوق',
    },
  },
  password_reset: {
    subject: { 'en-GB': 'Reset your SOUQ password', 'ar-EG': 'إعادة تعيين كلمة المرور' },
    text: {
      // Short expiry stated in the body: a reset link sitting in an inbox is a
      // standing key to the account, and saying so discourages forwarding.
      'en-GB': 'Hello {{name}},\n\nReset your password here — the link expires in 30 minutes:\n{{resetUrl}}\n\nIf you did not ask for this, ignore this email; nothing has changed.\n\nSOUQ',
      'ar-EG': 'أهلاً {{name}}،\n\nأعد تعيين كلمة المرور — الرابط صالح 30 دقيقة:\n{{resetUrl}}\n\nإذا لم تطلب ذلك، تجاهل الرسالة؛ لم يتغير شيء.\n\nسوق',
    },
  },
  email_verification: {
    subject: { 'en-GB': 'Confirm your email address', 'ar-EG': 'تأكيد بريدك الإلكتروني' },
    text: {
      'en-GB': 'Hello {{name}},\n\nConfirm your email:\n{{verifyUrl}}\n\nSOUQ',
      'ar-EG': 'أهلاً {{name}}،\n\nأكد بريدك الإلكتروني:\n{{verifyUrl}}\n\nسوق',
    },
  },
  security_alert: {
    subject: { 'en-GB': 'New sign-in to your SOUQ account', 'ar-EG': 'تسجيل دخول جديد لحسابك' },
    text: {
      'en-GB': 'Hello {{name}},\n\nSomeone signed in from {{location}} on {{device}} at {{when}}.\n\nIf this was you, nothing to do. If not, change your password now: {{securityUrl}}\n\nSOUQ',
      'ar-EG': 'أهلاً {{name}}،\n\nتم تسجيل الدخول من {{location}} عبر {{device}} في {{when}}.\n\nإن لم تكن أنت، غيّر كلمة المرور فوراً: {{securityUrl}}\n\nسوق',
    },
  },
  abandoned_cart: {
    subject: { 'en-GB': 'You left something behind', 'ar-EG': 'نسيت شيئاً في سلتك' },
    text: {
      'en-GB': 'Hello {{name}},\n\nYour cart is still waiting: {{cartUrl}}\n\nUnsubscribe: {{unsubscribeUrl}}\n\nSOUQ',
      'ar-EG': 'أهلاً {{name}}،\n\nسلتك في انتظارك: {{cartUrl}}\n\nإلغاء الاشتراك: {{unsubscribeUrl}}\n\nسوق',
    },
  },
  back_in_stock: {
    subject: { 'en-GB': '{{productTitle}} is back', 'ar-EG': '{{productTitle}} متوفر مجدداً' },
    text: {
      'en-GB': 'Hello {{name}},\n\n{{productTitle}} is available again: {{productUrl}}\n\nUnsubscribe: {{unsubscribeUrl}}\n\nSOUQ',
      'ar-EG': 'أهلاً {{name}}،\n\n{{productTitle}} متوفر مجدداً: {{productUrl}}\n\nإلغاء الاشتراك: {{unsubscribeUrl}}\n\nسوق',
    },
  },
};

/** RTL for Arabic, and inline CSS because email clients strip <style>. */
function wrapHtml(text: string, locale: string): string {
  const rtl = locale.startsWith('ar');
  const escaped = text
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/\n/g, '<br>');

  return `<!doctype html><html lang="${locale}" dir="${rtl ? 'rtl' : 'ltr'}"><body style="margin:0;padding:24px;background:#f6f6f6;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#111">
<div style="max-width:560px;margin:0 auto;background:#fff;border-radius:8px;padding:32px;text-align:${rtl ? 'right' : 'left'}">
<div style="font-size:20px;font-weight:600;margin-bottom:24px">SOUQ</div>
<div style="font-size:15px;line-height:1.6">${escaped}</div>
</div></body></html>`;
}

export function render(
  template: TemplateName,
  locale: string,
  params: Record<string, unknown>,
): { subject: string; text: string; html: string } {
  const t = TEMPLATES[template];
  if (!t) throw new Error(`unknown template: ${template}`);

  // Fall back to en-GB per FIELD, not per template: a partially translated
  // template should still send, in a mix, rather than not send at all.
  const pick = (m: Record<string, string>): string => {
    // Exact match first.
    const exact = m[locale];
    if (exact !== undefined) return exact;

    // Then any variant sharing the language. This direction is the one that
    // matters and it was missing: the templates are keyed 'ar-EG', so a user
    // whose locale is a bare 'ar' fell straight through to English. The old
    // `m[locale.slice(0, 2)]` only helped when the MAP held bare keys, which
    // it never does.
    const language = locale.slice(0, 2).toLowerCase();
    for (const [key, value] of Object.entries(m)) {
      if (key.slice(0, 2).toLowerCase() === language) return value;
    }

    return m['en-GB']!;
  };

  // escape:false on the text body — Mustache would HTML-escape an apostrophe
  // in a product title into &#39; in a plain-text email.
  const text = Mustache.render(pick(t.text), params, {}, { escape: (v) => String(v) });
  const subject = Mustache.render(pick(t.subject), params, {}, { escape: (v) => String(v) });

  return { subject, text, html: t.html ? Mustache.render(pick(t.html), params) : wrapHtml(text, locale) };
}

export function templateExists(name: string): name is TemplateName {
  // Object.hasOwn, NOT `name in TEMPLATES`.
  //
  // `in` walks the prototype chain, so `'__proto__' in TEMPLATES` is true, as
  // are 'constructor', 'toString' and every other Object.prototype member.
  // This function guards a lookup driven by a Kafka message field, so a caller
  // could pass `__proto__`, pass this check, and reach `TEMPLATES['__proto__']`
  // — which is Object.prototype, not a template, and blows up in render with
  // an error naming none of that.
  return Object.hasOwn(TEMPLATES, name);
}
