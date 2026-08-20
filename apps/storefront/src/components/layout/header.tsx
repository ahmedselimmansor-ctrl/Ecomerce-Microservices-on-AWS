import Link from 'next/link';
import { Menu, ShoppingBag, User } from 'lucide-react';

import { Button } from '@/components/ui/button';
import { Sheet, SheetContent, SheetHeader, SheetTitle, SheetTrigger } from '@/components/ui/sheet';
import { SearchBox } from './search-box';
import { CartCount } from '@/components/cart/cart-count';
import { AccountMenu } from '@/components/auth/account-menu';

/**
 * The site header.
 *
 * A server component. Only the three genuinely interactive pieces — search,
 * the cart count and the account menu — are client components, so the header's
 * markup is in the initial HTML and the navigation works before any JavaScript
 * has loaded.
 */
export function Header({ categories }: { categories: { slug: string; name: string }[] }) {
  return (
    <header className="sticky top-0 z-40 border-b bg-background/95 backdrop-blur supports-[backdrop-filter]:bg-background/80">
      {/*
        The skip link. First focusable element on the page, hidden until
        focused. Without it a keyboard user tabs through every category link on
        every page before reaching the content.
      */}
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:absolute focus:left-4 focus:top-4 focus:z-50 focus:rounded-md focus:bg-primary focus:px-4 focus:py-2 focus:text-primary-foreground"
      >
        Skip to content
      </a>

      <div className="container flex h-16 items-center gap-4">
        <Sheet>
          <SheetTrigger asChild>
            <Button variant="ghost" size="icon" className="md:hidden">
              <Menu className="h-5 w-5" />
              <span className="sr-only">Open menu</span>
            </Button>
          </SheetTrigger>
          <SheetContent side="left">
            <SheetHeader>
              <SheetTitle>Browse</SheetTitle>
            </SheetHeader>
            <nav className="mt-6">
              <ul className="space-y-1">
                {categories.map((category) => (
                  <li key={category.slug}>
                    <Link
                      href={`/search?category=${category.slug}`}
                      className="block rounded-md px-3 py-2.5 text-sm hover:bg-accent"
                    >
                      {category.name}
                    </Link>
                  </li>
                ))}
              </ul>
            </nav>
          </SheetContent>
        </Sheet>

        <Link href="/" className="shrink-0 text-lg font-bold tracking-tight">
          SOUQ
        </Link>

        <nav aria-label="Categories" className="hidden md:block">
          <ul className="flex items-center gap-1">
            {categories.slice(0, 5).map((category) => (
              <li key={category.slug}>
                <Link
                  href={`/search?category=${category.slug}`}
                  className="rounded-md px-3 py-2 text-sm text-muted-foreground transition-colors hover:text-foreground"
                >
                  {category.name}
                </Link>
              </li>
            ))}
          </ul>
        </nav>

        <div className="ml-auto flex max-w-md flex-1 items-center gap-2">
          <SearchBox />
        </div>

        <div className="flex shrink-0 items-center gap-1">
          <AccountMenu />

          <Button variant="ghost" size="icon" asChild className="relative">
            <Link href="/cart">
              <ShoppingBag className="h-5 w-5" />
              <CartCount />
              <span className="sr-only">Basket</span>
            </Link>
          </Button>
        </div>
      </div>
    </header>
  );
}

export function HeaderFallback() {
  return (
    <header className="sticky top-0 z-40 border-b bg-background">
      <div className="container flex h-16 items-center gap-4">
        <span className="text-lg font-bold tracking-tight">SOUQ</span>
        <div className="ml-auto flex items-center gap-1">
          <Button variant="ghost" size="icon" asChild>
            <Link href="/account">
              <User className="h-5 w-5" />
              <span className="sr-only">Account</span>
            </Link>
          </Button>
          <Button variant="ghost" size="icon" asChild>
            <Link href="/cart">
              <ShoppingBag className="h-5 w-5" />
              <span className="sr-only">Basket</span>
            </Link>
          </Button>
        </div>
      </div>
    </header>
  );
}
