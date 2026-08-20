import { OrderProgress } from '@/components/checkout/order-progress';

type Params = Promise<{ id: string }>;

export const metadata = {
  title: 'Your order',
  // Never index an order page. It is per-customer and the id is the only thing
  // guarding it from anyone who has the link.
  robots: { index: false, follow: false },
};

/**
 * Order status.
 *
 * Deliberately thin. Everything interesting is in `OrderProgress`, which is the
 * UI consequence of an asynchronous saga: `POST /v1/orders` returns **202**, so
 * at this moment the saga has started and has not finished. Rendering a
 * "thank you, your order is confirmed" page here would be a lie roughly one
 * time in ten.
 */
export default async function OrderStatusPage({ params }: { params: Params }) {
  const { id } = await params;

  return (
    <div className="container max-w-2xl py-12">
      <OrderProgress orderId={id} />
    </div>
  );
}
