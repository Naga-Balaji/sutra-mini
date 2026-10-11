// The browser calls /backend/..., and Next.js forwards it to the Go API.
// Same origin for the browser, so no CORS setup is needed in Go.
const API_URL = process.env.API_URL || "http://localhost:8080";

const nextConfig = {
  async rewrites() {
    return [{ source: "/backend/:path*", destination: `${API_URL}/:path*` }];
  },
};

export default nextConfig;
