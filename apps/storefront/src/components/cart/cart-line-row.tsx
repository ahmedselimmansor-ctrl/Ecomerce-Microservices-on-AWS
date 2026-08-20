'use client';

import Image from 'next/image';
import Link from 'next/link';
import { Trash2 } from 'lucide-react';

import { formatMoney, type CartLine } from '@souq/contracts';

import { Alert, AlertDescription } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import { QuantityStepper } from './quantity-stepper';
import { UnitPrice } from '@/components/catalog/price';
import { useCart } from './cart-provider';

export function CartLineRow({ line, locale = 'en-GB' }: { line: CartLine; locale?: string }) {
  const { setQuantity, remove, loading } = useCart();

  return (
    <li className="flex gap-4 py-5">
      <Link
        href={`/products/${line.productId}`}
        className="relative h-24 w-24 shrink-0 overflow-hidden rounded-md bg-muted"
      >
        {line.image ? (
          <Image src={line.image} alt="" fill sizes="96px" className="object-cover" />
        ) : (
          <span className="flex h-full items-center justify-center text-xs text-muted-foreground">
            No image
          </span>
        )}
      </Link>

      <div className="flex min-w-0 flex-1 flex-col gap-2">
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <h3 className="text-sm font-medium leading-snug">
              <Link href={`/products/${line.productId}`} className="hover:underline">
                {line.title}
              </Link>
            </h3>
            <p className="mt-0.5 text-xs text-muted-foreground">{line.sku}</p>
          </div>

          <div className="shrink-0 text-right">
            <p className="tabular font-semibold">{formatMoney(line.lineTotal, locale)}</p>
            <UnitPrice unitPrice={line.unitPrice} quantity={line.quantity} locale={locale} />
          </div>
        </div>

        {/*
          `priceChanged` is set by cart-service when the price moved after the
          line was added. Surfacing it is not optional — charging a different
          number from the one someone chose to add, without saying so, is the
          complaint that becomes a chargeback.
        */}
        {line.priceChanged && (
          <Alert variant="warning" className="py-2">
            <AlertDescription className="text-xs">
              The price of this item changed since you added it.
            </AlertDescription>
          </Alert>
        )}

        <div className="flex items-center gap-2">
          <QuantityStepper
            value={line.quantity}
            onCommit={(q) => setQuantity(line.sku, q)}
            disabled={loading}
            label={`Quantity of ${line.title}`}
          />

          <Button
            variant="ghost"
            size="icon"
            disabled={loading}
            onClick={() => remove(line.sku)}
            className="text-muted-foreground hover:text-destructive"
          >
            <Trash2 className="h-4 w-4" />
            <span className="sr-only">Remove {line.title} from your basket</span>
          </Button>
        </div>
      </div>
    </li>
  );
}
