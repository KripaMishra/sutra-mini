import { API_BASE } from "@/lib/api";

// Server-side proxy for the retry action: the browser calls this route, this
// route calls the Go API. Keeps the API free of CORS configuration.
export async function POST(_request: Request, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  if (!/^\d+$/.test(id)) {
    return Response.json({ error: "invalid voucher id" }, { status: 400 });
  }

  try {
    const res = await fetch(`${API_BASE}/vouchers/${id}/retry`, { method: "POST", cache: "no-store" });
    const body = await res.json().catch(() => ({}));
    return Response.json(body, { status: res.status });
  } catch {
    return Response.json({ error: "the API is not reachable" }, { status: 503 });
  }
}
