import { Suspense } from "react";
import { fetchVouchers, formatINR, timeAgo, type Voucher, type VoucherStatus } from "@/lib/api";
import { RefreshControl, RetryButton } from "./controls";

export default function SyncPage() {
  return (
    <main className="mx-auto w-full max-w-4xl px-6 py-10">
      <header className="mb-8 flex flex-wrap items-end justify-between gap-4">
        <div>
          <p className="text-xs font-medium uppercase tracking-[0.2em] text-slate-400">Sutra Mini</p>
          <h1 className="mt-1 text-2xl font-semibold tracking-tight">Voucher sync</h1>
          <p className="mt-1 text-sm text-slate-500">
            Receipts become vouchers; the worker delivers them to accounting exactly once.
          </p>
        </div>
        <RefreshControl />
      </header>

      <Suspense fallback={<VouchersSkeleton />}>
        <Vouchers />
      </Suspense>
    </main>
  );
}

// Vouchers streams in per request; the header and shell render instantly.
async function Vouchers() {
  const { vouchers, error } = await fetchVouchers();

  // An outage must not render as "all clear": no zeroed stats, no empty
  // sections — just the reason and a way back.
  if (error) {
    return (
      <>
        <div className="mb-6 rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-700">
          {error}
        </div>
        <p className="rounded-xl border border-dashed border-slate-200 px-4 py-6 text-sm text-slate-400">
          Voucher data is unavailable right now — fix the API connection, then press R or Refresh.
        </p>
      </>
    );
  }

  const groups: Record<VoucherStatus, Voucher[]> = { failed: [], pending: [], pushed: [] };
  for (const voucher of vouchers) {
    groups[voucher.status]?.push(voucher);
  }
  const sections: VoucherStatus[] = ["failed", "pending", "pushed"];
  const sum = (list: Voucher[]) => list.reduce((n, v) => n + v.amount_paise, 0);

  return (
    <>
      <p className="mb-4 text-xs tabular-nums text-slate-400">
        As of {new Date().toLocaleString("en-IN")}
      </p>

      <section className="mb-8 grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Stat label="Vouchers" count={vouchers.length} amountPaise={sum(vouchers)} tone="slate" />
        <Stat label="Pending" count={groups.pending.length} amountPaise={sum(groups.pending)} tone="amber" />
        <Stat label="Pushed" count={groups.pushed.length} amountPaise={sum(groups.pushed)} tone="emerald" />
        <Stat label="Failed" count={groups.failed.length} amountPaise={sum(groups.failed)} tone="rose" />
      </section>

      {sections.map((status) => (
        <section key={status} className="mb-8">
          <div className="mb-3 flex items-center gap-2">
            <StatusDot status={status} />
            <h2 className="text-sm font-semibold uppercase tracking-wider text-slate-600">{status}</h2>
            <span className="text-xs tabular-nums text-slate-400">({groups[status].length})</span>
          </div>
          {groups[status].length === 0 ? (
            <p className="rounded-xl border border-dashed border-slate-200 px-4 py-6 text-sm text-slate-400">
              No {status} vouchers.
            </p>
          ) : (
            <ul className="flex flex-col gap-2">
              {groups[status].map((voucher) => (
                <VoucherRow key={voucher.id} voucher={voucher} />
              ))}
            </ul>
          )}
        </section>
      ))}
    </>
  );
}

function VouchersSkeleton() {
  return (
    <div className="animate-pulse">
      <div className="mb-8 grid grid-cols-2 gap-3 sm:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <div key={i} className="h-[86px] rounded-xl border border-slate-200 bg-white shadow-sm" />
        ))}
      </div>
      <div className="flex flex-col gap-2">
        {[0, 1, 2].map((i) => (
          <div key={i} className="h-[66px] rounded-xl border border-slate-200 bg-white shadow-sm" />
        ))}
      </div>
    </div>
  );
}

function VoucherRow({ voucher }: { voucher: Voucher }) {
  const updated = timeAgo(voucher.updated_at);
  return (
    <li className="rounded-xl border border-slate-200 bg-white px-4 py-3 shadow-sm">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <span className="font-mono text-sm text-slate-700">{voucher.client_ref}</span>
            <span className="rounded-full bg-slate-100 px-2 py-0.5 text-[11px] tabular-nums text-slate-500">
              #{voucher.id}
            </span>
          </div>
          <p className="mt-0.5 text-xs text-slate-400">
            {voucher.type} · attempt{voucher.attempts === 1 ? "" : "s"}: {voucher.attempts}
            {voucher.external_ref ? (
              <>
                {" "}
                · accounting ref <span className="font-mono">{voucher.external_ref}</span>
              </>
            ) : null}
            {updated ? <> · updated {updated}</> : null}
          </p>
        </div>
        <div className="flex items-center gap-4">
          <span className="text-sm font-semibold tabular-nums text-slate-800">
            {formatINR(voucher.amount_paise)}
          </span>
          {voucher.status === "failed" ? <RetryButton voucherId={voucher.id} /> : null}
        </div>
      </div>
      {voucher.status === "failed" && voucher.last_error ? (
        <p className="mt-2 rounded-lg bg-rose-50 px-3 py-2 font-mono text-xs text-rose-700">{voucher.last_error}</p>
      ) : null}
      {voucher.status === "pending" && voucher.last_error ? (
        <p className="mt-2 rounded-lg bg-amber-50 px-3 py-2 font-mono text-xs text-amber-700">
          last attempt: {voucher.last_error}
        </p>
      ) : null}
    </li>
  );
}

function StatusDot({ status }: { status: VoucherStatus }) {
  const tone =
    status === "failed" ? "bg-rose-500" : status === "pending" ? "bg-amber-500" : "bg-emerald-500";
  return <span className={`h-2 w-2 rounded-full ${tone}`} />;
}

function Stat({
  label,
  count,
  amountPaise,
  tone,
}: {
  label: string;
  count: number;
  amountPaise: number;
  tone: "slate" | "amber" | "emerald" | "rose";
}) {
  const tones = {
    slate: "text-slate-900",
    amber: "text-amber-600",
    emerald: "text-emerald-600",
    rose: "text-rose-600",
  };
  return (
    <div className="rounded-xl border border-slate-200 bg-white px-4 py-3 shadow-sm">
      <p className="text-[11px] font-medium uppercase tracking-wider text-slate-400">{label}</p>
      <p className={`mt-1 text-xl font-semibold tabular-nums ${tones[tone]}`}>{count}</p>
      <p className="text-xs tabular-nums text-slate-400">{formatINR(amountPaise)}</p>
    </div>
  );
}
