import { API_BASE } from "@/lib/api";

// Server-side proxy for the retry action: the browser calls this route, this
// route calls the Go API. Keeps the API free of CORS configuration.
export async function POST(_request: Request, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  if (!/^\d+$/.test(id)) {
    return Response.json({ error: "invalid voucher id" }, { status: 400 });
  }

  let res: Response;
  try {
    res = await fetch(`${API_BASE}/vouchers/${id}/retry`, {
      method: "POST",
      cache: "no-store",
      signal: AbortSignal.timeout(8000),
    });
  } catch {
    return Response.json({ error: "the API is not reachable" }, { status: 503 });
  }

  // Null-body statuses cannot carry JSON; pass them through untouched.
  if (res.status === 204 || res.status === 205 || res.status === 304) {
    return new Response(null, { status: res.status });
  }

  // Non-JSON error bodies (a proxy page, a panic trace) must not be dropped.
  const text = await res.text();
  try {
    return Response.json(JSON.parse(text), { status: res.status });
  } catch {
    return Response.json({ error: text.trim() || `HTTP ${res.status}` }, { status: res.status });
  }
}
