import { ProductCardSkeleton, ProductGrid } from '@/components/catalog/product-card';
import { Skeleton } from '@/components/ui/skeleton';

export default function Loading() {
  return (
    <div className="container py-8">
      <Skeleton className="h-8 w-56" />
      <div className="mt-6">
        <ProductGrid>
          {Array.from({ length: 10 }, (_, i) => (
            <ProductCardSkeleton key={i} />
          ))}
        </ProductGrid>
      </div>
    </div>
  );
}
