"use client";

export default function SyncError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  return (
    <main className="mx-auto w-full max-w-4xl px-6 py-10">
      <div className="rounded-xl border border-rose-200 bg-rose-50 px-4 py-6 text-sm text-rose-700">
        <p className="font-medium">Something went wrong rendering this page.</p>
        <p className="mt-1 break-all font-mono text-xs">{error.message}</p>
        <button
          type="button"
          onClick={() => reset()}
          className="mt-4 rounded-lg bg-slate-900 px-3 py-1.5 text-sm font-medium text-white shadow-sm transition-colors hover:bg-slate-700"
        >
          Try again
        </button>
      </div>
    </main>
  );
}
