/**
 * @type {import('next').NextConfig}
 *
 * The admin app is locked down harder than the storefront, and the differences
 * are all deliberate.
 *
 * `noindex` on every response. There is no scenario in which an internal
 * operations tool should appear in a search index, and relying on each page to
 * remember its own robots meta is how one of them forgets.
 *
 * No image optimiser allow-list, because the admin renders product images from
 * the same CDN and nothing else. Anything broader would let an admin-supplied
 * URL turn this origin into a proxy for hosts the pod can reach and the
 * internet cannot.
 */
const nextConfig = {
  reactStrictMode: true,
  poweredByHeader: false,
  output: 'standalone',

  images: {
    remotePatterns: [
      { protocol: 'https', hostname: 'cdn.souq.dev' },
      { protocol: 'https', hostname: 'souq-media.s3.amazonaws.com' },
      { protocol: 'http', hostname: 'localhost', port: '4566' },
    ],
  },

  transpilePackages: ['@souq/contracts'],

  async headers() {
    return [
      {
        source: '/:path*',
        headers: [
          { key: 'X-Content-Type-Options', value: 'nosniff' },
          { key: 'X-Frame-Options', value: 'DENY' },
          // No referrer at all. An admin URL carries order ids and customer
          // ids in the path, and leaking those to any external host an admin
          // clicks through to is a data disclosure.
          { key: 'Referrer-Policy', value: 'no-referrer' },
          { key: 'X-Robots-Tag', value: 'noindex, nofollow, noarchive' },
          { key: 'Permissions-Policy', value: 'camera=(), microphone=(), geolocation=(), payment=()' },
          {
            key: 'Content-Security-Policy',
            value: [
              "default-src 'self'",
              "script-src 'self' 'unsafe-inline'",
              "style-src 'self' 'unsafe-inline'",
              "img-src 'self' data: blob: https://cdn.souq.dev https://souq-media.s3.amazonaws.com",
              "font-src 'self' data:",
              "connect-src 'self'",
              "frame-ancestors 'none'",
              "form-action 'self'",
              "base-uri 'self'",
              "object-src 'none'",
            ].join('; '),
          },
        ],
      },
    ];
  },
};

export default nextConfig;
