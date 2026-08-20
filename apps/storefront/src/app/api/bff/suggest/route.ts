import { NextResponse } from 'next/server';

import { SuggestResponse } from '@souq/contracts';

import { call } from '@/lib/bff';
import { problemResponse } from '../_problem';

/**
 * Search type-ahead.
 *
 * The only BFF route that is cached, and it is cached deliberately: suggestions
 * are identical for every visitor, they are requested on nearly every keystroke,
 * and thirty seconds at the edge removes almost all of that load from
 * search-service.
 *
 * A failure returns an **empty list with a 200**, not an error. Suggestions are
 * a convenience; a 500 here would make the search box show an error for
 * something the user never asked for, while the form itself still works.
 */
export async function GET(request: Request) {
  const query = new URL(request.url).searchParams.get('q')?.trim() ?? '';

  if (query.length < 2) {
    return NextResponse.json({ suggestions: [] });
  }

  try {
    const result = await call({
      service: 'search',
      // Bounded before it leaves this process. An unbounded query string
      // reaches Elasticsearch's query parser, and a very long one is a cheap
      // way to make it work hard.
      path: `/v1/suggest?q=${encodeURIComponent(query.slice(0, 100))}`,
      schema: SuggestResponse,
      revalidate: 30,
    });

    return NextResponse.json(result, {
      headers: { 'Cache-Control': 'public, max-age=30, stale-while-revalidate=120' },
    });
  } catch (error) {
    console.warn('[bff] suggest failed, returning nothing', error);
    return NextResponse.json({ suggestions: [] });
  }
}
