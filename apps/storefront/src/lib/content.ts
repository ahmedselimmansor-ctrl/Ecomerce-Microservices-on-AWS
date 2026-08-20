/**
 * Legal and help copy.
 *
 * In the repository rather than in a CMS, deliberately. These pages state what
 * the platform actually does — a 30-day return window, a 15-minute access
 * token, a basket that expires after 30 days — and every one of those numbers
 * also exists in the code. A CMS lets someone change the stated policy without
 * changing the behaviour, and the gap between the two is what a regulator reads.
 *
 * `updated` is the date the text last changed, not the build date. A privacy
 * notice whose date moves on every deploy tells a reader nothing.
 */

export interface ContentSection {
  heading?: string;
  paragraphs?: string[];
  bullets?: string[];
}

export interface ContentPage {
  slug: string;
  title: string;
  updated: string;
  summary: string;
  body: ContentSection[];
}

export const LEGAL: ContentPage[] = [
  {
    slug: 'privacy',
    title: 'Privacy',
    updated: '2026-08-18',
    summary: 'What we collect, why, and how long we keep it.',
    body: [
      {
        paragraphs: [
          'This describes what the platform actually does. Where a retention period appears below, it is the same number configured in the system — not an aspiration.',
        ],
      },
      {
        heading: 'What we hold',
        bullets: [
          'Your email address, name and locale, so we can identify you and write to you.',
          'A hash of your password — Argon2id, never the password itself. We cannot read it, and neither can anyone who obtains our database.',
          'Your orders, addresses and payment outcomes.',
          'Sign-in attempts, including the IP address, for 30 days. This is how we notice someone trying passwords against your account.',
        ],
      },
      {
        heading: 'What we do not hold',
        bullets: [
          'Card numbers, expiry dates or security codes. Card details are collected by Paymob in a hosted field and never reach our servers.',
          'Your password in any recoverable form.',
        ],
      },
      {
        heading: 'How long',
        bullets: [
          'Sign-in attempts: 30 days.',
          'Sessions: a sign-in lasts 30 days unless you sign out. Each request uses a token valid for 15 minutes, refreshed automatically.',
          'Orders: seven years, because tax law requires it.',
          'An abandoned basket: 30 days.',
        ],
      },
      {
        heading: 'Your rights',
        paragraphs: [
          'You can ask for a copy of your data, ask us to correct it, or ask us to delete it. Deletion removes your profile and addresses. It cannot remove orders we are legally required to keep, and we will tell you which those are rather than quietly keeping them.',
        ],
      },
    ],
  },
  {
    slug: 'terms',
    title: 'Terms',
    updated: '2026-08-18',
    summary: 'The agreement between you and us.',
    body: [
      {
        heading: 'Orders',
        paragraphs: [
          'Placing an order is an offer to buy. We accept it when we confirm the order, which is also the moment stock is committed to you — not when you press the button.',
          'That gap is real and it is why the confirmation is not instant. Between placing and confirming we reserve stock and authorise payment; if either fails the order is cancelled and nothing is charged. You will see which happened on the order page within a few seconds.',
        ],
      },
      {
        heading: 'Prices',
        paragraphs: [
          'The price shown at checkout is the price you pay. If a price changes between adding an item to your basket and checking out, we tell you before you confirm.',
          'Where a previous price is shown struck through, it is a price the item was actually sold at.',
        ],
      },
      {
        heading: 'Cancellation and returns',
        bullets: [
          'You can cancel any time before we confirm the order.',
          'After confirmation, return anything within 30 days for a refund.',
          'Refunds go back to the original payment method.',
        ],
      },
    ],
  },
];

export const HELP: ContentPage[] = [
  {
    slug: 'delivery',
    title: 'Delivery',
    updated: '2026-08-18',
    summary: 'Timescales and costs.',
    body: [
      {
        bullets: [
          'Standard delivery is free on orders over EGP 500.',
          'Orders placed before 2pm are dispatched the same working day.',
          'You will get a tracking number when the order ships.',
        ],
      },
      {
        heading: 'If your order says “Processing”',
        paragraphs: [
          'Placing an order starts a sequence across several systems: reserving stock, authorising payment, then committing the stock. It normally finishes in a few seconds. If it is still processing after a minute the order page will tell you what to do — and nothing has been charged unless it says so.',
        ],
      },
    ],
  },
  {
    slug: 'returns',
    title: 'Returns',
    updated: '2026-08-18',
    summary: 'Thirty days, free.',
    body: [
      {
        bullets: [
          'Thirty days from delivery, for any reason.',
          'The item should be in a condition you would be happy to receive it in.',
          'Return postage is on us.',
          'Refunds reach your account within five working days of the item arriving with us.',
        ],
      },
    ],
  },
  {
    slug: 'contact',
    title: 'Contact us',
    updated: '2026-08-18',
    summary: 'How to reach a person.',
    body: [
      {
        paragraphs: ['Email support@souq.dev. We answer within one working day.'],
      },
      {
        heading: 'If something went wrong with an order',
        paragraphs: [
          'Include the order reference. If you saw an error message with a reference code in it, include that too — it points at the exact request in our logs and saves a round of questions.',
        ],
      },
    ],
  },
];

export function findContent(collection: ContentPage[], slug: string): ContentPage | undefined {
  return collection.find((page) => page.slug === slug);
}
