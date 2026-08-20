import type { ContentPage } from '@/lib/content';

/**
 * Renders a content page from structured data rather than from a Markdown or
 * HTML string.
 *
 * No `dangerouslySetInnerHTML` anywhere. These pages contain no user input
 * today, and building them out of arrays of strings means they cannot start
 * containing it later without someone changing the type.
 */
export function ContentBody({ page }: { page: ContentPage }) {
  return (
    <>
      <header className="not-prose">
        <h1 className="text-2xl font-bold tracking-tight">{page.title}</h1>
        <p className="mt-1 text-sm text-muted-foreground">{page.summary}</p>
        {/*
          The date the text changed, not the build date. A policy page whose
          "last updated" moves on every deploy is a page whose date means
          nothing — and for a privacy notice, that date is the thing a reader
          checks first.
        */}
        <p className="mt-2 text-xs text-muted-foreground">
          Last updated{' '}
          <time dateTime={page.updated}>
            {new Date(page.updated).toLocaleDateString('en-GB', {
              year: 'numeric', month: 'long', day: 'numeric',
            })}
          </time>
        </p>
      </header>

      {page.body.map((section, index) => (
        <section key={section.heading ?? index}>
          {section.heading && <h2>{section.heading}</h2>}

          {section.paragraphs?.map((paragraph) => (
            <p key={paragraph} className="mt-2 text-muted-foreground">
              {paragraph}
            </p>
          ))}

          {section.bullets && (
            <ul className="mt-2 text-muted-foreground">
              {section.bullets.map((bullet) => (
                <li key={bullet}>{bullet}</li>
              ))}
            </ul>
          )}
        </section>
      ))}
    </>
  );
}
