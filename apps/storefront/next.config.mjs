/**
 * @type {import('next').NextConfig}
 *
 * Two settings here are security rather than configuration.
 *
 * `images.remotePatterns` is an allow-list. Next's image optimiser will fetch
 * and re-serve any URL it is given, so an open configuration turns this app
 * into a proxy anyone can point at any host — including internal ones the pod
 * can reach and the internet cannot.
 *
 * The CSP has no `unsafe-eval` and no wildcard. It is the last line of defence
 * against XSS, and the value of that defence is exactly the value of the
 * narrowest directive in it.
 */
const nextConfig = {
  reactStrictMode: true,
  poweredByHeader: false,

  // Traces the real import graph and emits a server bundle with only what it
  // needs — roughly a tenth of the image size of copying node_modules.
  output: 'standalone',

  images: {
    remotePatterns: [
      { protocol: 'https', hostname: 'cdn.souq.dev' },
      { protocol: 'https', hostname: 'souq-media.s3.amazonaws.com' },
      // Local development only; the seed data points here.
      { protocol: 'http', hostname: 'localhost', port: '4566' },
    ],
    formats: ['image/avif', 'image/webp'],
  },

  experimental: {
    // Transpile the shared contracts package from source rather than requiring
    // a build step before `next dev` works.
    optimizePackageImports: ['lucide-react'],
  },

  transpilePackages: ['@souq/contracts'],

  async headers() {
    return [
      {
        source: '/:path*',
        headers: [
          { key: 'X-Content-Type-Options', value: 'nosniff' },
          { key: 'Referrer-Policy', value: 'strict-origin-when-cross-origin' },
          // Deny rather than sameorigin: nothing in this app is meant to be
          // framed, and clickjacking a checkout button is the whole attack.
          { key: 'X-Frame-Options', value: 'DENY' },
          { key: 'Permissions-Policy', value: 'camera=(), microphone=(), geolocation=(), payment=()' },
          {
            key: 'Content-Security-Policy',
            value: [
              "default-src 'self'",
              // Next injects inline bootstrap scripts, so 'unsafe-inline' is
              // unavoidable without nonce plumbing through every route. There
              // is deliberately no 'unsafe-eval'.
              "script-src 'self' 'unsafe-inline'",
              "style-src 'self' 'unsafe-inline'",
              "img-src 'self' data: blob: https://cdn.souq.dev https://souq-media.s3.amazonaws.com",
              "font-src 'self' data:",
              // Same-origin only. The browser never calls a service directly
              // (docs/CONTRACTS.md §8), so there is nothing else to allow.
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
