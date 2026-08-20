export default function ContentLayout({ children }: { children: React.ReactNode }) {
  return (
    <div className="container max-w-2xl py-12">
      {/*
        A max-width in the sixties of characters. Prose wider than that costs
        the reader accuracy returning to the start of each line — the reason
        every newspaper column is narrow, and the single cheapest thing you can
        do for a page of text.
      */}
      <article className="space-y-4 text-sm leading-relaxed [&_h2]:mt-8 [&_h2]:text-base [&_h2]:font-semibold [&_ul]:list-disc [&_ul]:space-y-1 [&_ul]:pl-5">
        {children}
      </article>
    </div>
  );
}
