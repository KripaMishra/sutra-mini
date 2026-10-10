// Server-side helpers for the Sutra API. The browser never talks to the Go
// API directly — client components go through the /api/retry proxy — so the
// API needs no CORS configuration.

export const API_BASE = (process.env.API_BASE_URL ?? "http://localhost:8080").replace(/\/+$/, "");

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
  try {
    const res = await fetch(`${API_BASE}/vouchers`, { cache: "no-store" });
    if (!res.ok) {
      return { vouchers: [], error: `The API answered HTTP ${res.status}.` };
    }
    const data = (await res.json()) as { vouchers: Voucher[] | null };
    return { vouchers: data.vouchers ?? [], error: null };
  } catch {
    return {
      vouchers: [],
      error: `Cannot reach the API at ${API_BASE}. Start it with: go run ./cmd/api`,
    };
  }
}

export function formatINR(paise: number): string {
  return new Intl.NumberFormat("en-IN", {
    style: "currency",
    currency: "INR",
    minimumFractionDigits: 2,
  }).format(paise / 100);
}

export function timeAgo(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const seconds = Math.max(0, Math.round((Date.now() - then) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 48) return `${hours}h ago`;
  return new Date(iso).toLocaleDateString("en-IN");
}
