import { CheckoutFlow } from '@/components/checkout/checkout-flow';

export const metadata = {
  title: 'Checkout',
  robots: { index: false, follow: false },
};

/**
 * Checkout.
 *
 * The page itself is a shell. Everything is client-side because the basket,
 * the address form and the payment token are all per-session state that must
 * never be rendered on a cacheable server response.
 */
export default function CheckoutPage() {
  return (
    <div className="container max-w-4xl py-8">
      <h1 className="text-2xl font-bold tracking-tight">Checkout</h1>
      <CheckoutFlow />
    </div>
  );
}
