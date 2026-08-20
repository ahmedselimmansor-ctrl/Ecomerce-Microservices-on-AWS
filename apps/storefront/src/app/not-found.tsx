import Link from 'next/link';

import { Button } from '@/components/ui/button';

export default function NotFound() {
  return (
    <div className="container flex flex-col items-center py-24 text-center">
      <p className="text-sm font-semibold text-muted-foreground">404</p>
      <h1 className="mt-2 text-2xl font-bold tracking-tight">We could not find that page</h1>
      <p className="mt-2 max-w-md text-sm text-muted-foreground">
        The link may be out of date, or the product may have been discontinued.
      </p>
      <div className="mt-6 flex gap-3">
        <Button asChild>
          <Link href="/">Go home</Link>
        </Button>
        <Button variant="outline" asChild>
          <Link href="/search">Browse everything</Link>
        </Button>
      </div>
    </div>
  );
}
