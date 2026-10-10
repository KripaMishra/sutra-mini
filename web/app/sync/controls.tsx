"use client";

import { useRouter } from "next/navigation";
import { useCallback, useEffect, useState, useTransition } from "react";

// Refresh re-renders the server component; R does the same from the keyboard.
export function RefreshControl() {
  const router = useRouter();
  const [pending, startTransition] = useTransition();

  const refresh = useCallback(() => {
    startTransition(() => router.refresh());
  }, [router]);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key.toLowerCase() !== "r" || event.repeat || event.defaultPrevented) return;
      if (event.metaKey || event.ctrlKey || event.altKey) return;
      const target = event.target as HTMLElement | null;
      if (target?.closest("input, textarea, select, [contenteditable]")) return;
      refresh();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [refresh]);

  return (
    <button
      type="button"
      onClick={refresh}
      title="Refresh (R)"
      aria-label="Refresh"
      className="inline-flex items-center gap-2 rounded-lg border border-slate-200 bg-white px-3 py-1.5 text-sm font-medium text-slate-700 shadow-sm transition-colors hover:bg-slate-50 active:bg-slate-100"
    >
      {pending ? "Refreshing…" : "Refresh"}
      <kbd
        aria-hidden
        className="rounded border border-slate-200 bg-slate-50 px-1.5 py-0.5 font-mono text-[10px] text-slate-400"
      >
        R
      </kbd>
    </button>
  );
}

// Retry goes through the same-origin /api/retry proxy so the API stays CORS-free.
export function RetryButton({ voucherId }: { voucherId: number }) {
  const router = useRouter();
  const [busy, setBusy] = useState(false);
  const [isPending, startTransition] = useTransition();
  const [error, setError] = useState<string | null>(null);

  async function retry() {
    setBusy(true);
    setError(null);
    try {
      const res = await fetch(`/api/retry/${voucherId}`, { method: "POST" });
      if (!res.ok) {
        const body = (await res.json().catch(() => null)) as { error?: string } | null;
        throw new Error(body?.error ?? `HTTP ${res.status}`);
      }
      // Stay disabled until the refreshed row actually lands, so a second
      // click cannot race the requeue and report a fake 409.
      startTransition(() => router.refresh());
    } catch (err) {
      setError(err instanceof Error ? err.message : "retry failed");
    } finally {
      setBusy(false);
    }
  }

  const disabled = busy || isPending;
  return (
    <span className="inline-flex items-center gap-2">
      <button
        type="button"
        onClick={retry}
        disabled={disabled}
        aria-busy={disabled}
        className="rounded-lg bg-slate-900 px-3 py-1.5 text-sm font-medium text-white shadow-sm transition-colors hover:bg-slate-700 disabled:cursor-not-allowed disabled:opacity-50"
      >
        {disabled ? "Retrying…" : "Retry"}
      </button>
      {error ? (
        <span role="status" className="text-xs text-rose-600">
          {error}
        </span>
      ) : null}
    </span>
  );
}
