import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  /* config options here */
  cacheComponents: true,
  partialPrefetching: true,
  // "/" is a redirect, not a page: expressed here it renders nothing, so there
  // is no segment for instant-navigation validation to trip over. permanent:
  // false keeps the 307 the page used to send.
  async redirects() {
    return [{ source: "/", destination: "/sync", permanent: false }];
  },
  turbopack: {
    rules: {
      "*.css": {
        loaders: ["@tailwindcss/turbopack"],
        as: "*.css",
      },
    },
  },
};

export default nextConfig;
