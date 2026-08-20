'use client';

import Link from 'next/link';
import { LogOut, Package, Settings, User } from 'lucide-react';

import { Button } from '@/components/ui/button';
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem,
  DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { useSession } from './session-provider';

export function AccountMenu() {
  const { user, loading, signOut } = useSession();

  // While the session resolves, render the signed-out affordance rather than a
  // spinner. It is right most of the time, it is the correct size, and it does
  // not make the header jump when the answer arrives.
  if (loading || !user) {
    return (
      <Button variant="ghost" size="icon" asChild>
        <Link href="/login">
          <User className="h-5 w-5" />
          <span className="sr-only">Sign in</span>
        </Link>
      </Button>
    );
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon">
          <User className="h-5 w-5" />
          <span className="sr-only">Account menu for {user.fullName}</span>
        </Button>
      </DropdownMenuTrigger>

      <DropdownMenuContent align="end" className="w-56">
        <DropdownMenuLabel className="font-normal">
          <p className="truncate text-sm font-medium">{user.fullName}</p>
          <p className="truncate text-xs text-muted-foreground">{user.email}</p>
        </DropdownMenuLabel>

        <DropdownMenuSeparator />

        <DropdownMenuItem asChild>
          <Link href="/account/orders">
            <Package className="h-4 w-4" />
            Your orders
          </Link>
        </DropdownMenuItem>

        <DropdownMenuItem asChild>
          <Link href="/account">
            <Settings className="h-4 w-4" />
            Account settings
          </Link>
        </DropdownMenuItem>

        {/*
          Shown only to staff, and only as a shortcut. The admin app does its
          own authorisation — it requires an MFA session (CONTRACTS §7), which
          this link cannot and does not grant.
        */}
        {(user.roles.includes('ADMIN') || user.roles.includes('OPS')) && (
          <>
            <DropdownMenuSeparator />
            <DropdownMenuItem asChild>
              <a href={process.env.NEXT_PUBLIC_ADMIN_URL ?? 'http://localhost:3001'}>
                Admin dashboard
              </a>
            </DropdownMenuItem>
          </>
        )}

        <DropdownMenuSeparator />

        <DropdownMenuItem onSelect={() => void signOut()}>
          <LogOut className="h-4 w-4" />
          Sign out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
