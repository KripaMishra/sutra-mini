// Server-side helpers for the Sutra API. The browser never talks to the Go
// API directly — client components go through the /api/retry proxy — so the
// API needs no CORS configuration.

export const API_BASE = (process.env.API_BASE_URL ?? "http://localhost:8080").replace(/\/+$/, "");

// Comfortably above the mock accounting API's 5s hang.
const FETCH_TIMEOUT_MS = 8000;

export type VoucherStatus = "pending" | "pushed" | "failed";

export type Voucher = {
  id: number;
  type: string;
  source_id: number;
  amount_paise: number;
  status: VoucherStatus;
  attempts: number;
  last_error: string;
  client_ref: string;
  external_ref: string;
  next_attempt_at: string;
  created_at: string;
  updated_at: string;
};

export async function fetchVouchers(): Promise<{ vouchers: Voucher[]; error: string | null }> {
  let res: Response;
  try {
    res = await fetch(`${API_BASE}/vouchers`, {
      cache: "no-store",
      signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
    });
  } catch (err) {
    console.error("fetchVouchers: request failed", err);
    return { vouchers: [], error: "The API is unreachable — is the Go API running on port 8080?" };
  }

  if (!res.ok) {
    return { vouchers: [], error: `The API answered HTTP ${res.status}.` };
  }

  try {
    const data = (await res.json()) as { vouchers: Voucher[] | null };
    return { vouchers: data.vouchers ?? [], error: null };
  } catch (err) {
    console.error("fetchVouchers: unreadable response body", err);
    return { vouchers: [], error: "The API returned an unreadable response." };
  }
}

export function formatINR(paise: number): string {
  if (!Number.isFinite(paise)) return "—";
  return new Intl.NumberFormat("en-IN", {
    style: "currency",
    currency: "INR",
    minimumFractionDigits: 2,
  }).format(paise / 100);
}

export function timeAgo(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const seconds = Math.round((Date.now() - then) / 1000);
  if (seconds < 5) return "just now"; // future timestamps (clock skew) land here too
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 48) return `${hours}h ago`;
  return new Date(iso).toLocaleDateString("en-IN");
}
