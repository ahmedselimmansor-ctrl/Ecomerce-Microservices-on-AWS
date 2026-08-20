import { OrderHistory } from '@/components/checkout/order-history';

export const metadata = {
  title: 'Your orders',
  robots: { index: false, follow: false },
};

export default function OrdersPage() {
  return <OrderHistory />;
}
