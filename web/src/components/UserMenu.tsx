import { useEffect, useId, useRef, useState } from "react";
import type { Me } from "../api/types";

// Monogram background colors are sampled deterministically from the user's
// name so the same person always sees the same color across reloads and
// machines -- a one-line stable identity cue, no backend round trip.
const MONOGRAM_COLORS = [
  "bg-emerald-700 text-emerald-100",
  "bg-blue-700 text-blue-100",
  "bg-amber-700 text-amber-100",
  "bg-purple-700 text-purple-100",
  "bg-rose-700 text-rose-100",
  "bg-teal-700 text-teal-100",
  "bg-indigo-700 text-indigo-100",
  "bg-fuchsia-700 text-fuchsia-100",
] as const;

function monogramColorClass(name: string): string {
  let h = 0;
  for (let i = 0; i < name.length; i++) {
    h = (h * 31 + name.charCodeAt(i)) >>> 0;
  }
  return MONOGRAM_COLORS[h % MONOGRAM_COLORS.length];
}

function monogramInitials(name: string): string {
  const trimmed = name.trim();
  if (!trimmed) return "?";
  const parts = trimmed.split(/\s+/);
  const first = parts[0]?.[0] ?? "";
  const second = parts.length > 1 ? parts[parts.length - 1]?.[0] ?? "" : "";
  return (first + second).toUpperCase();
}

interface UserMenuProps {
  me: Me;
}

export function UserMenu({ me }: UserMenuProps) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const dialogId = useId();

  useEffect(() => {
    if (!open) return;
    const onPointer = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setOpen(false);
        triggerRef.current?.focus();
      }
    };
    document.addEventListener("mousedown", onPointer);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onPointer);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  const initials = monogramInitials(me.name ?? "");
  const colorClass = monogramColorClass(me.name ?? "");
  const groups = me.groups ?? [];

  return (
    <div ref={rootRef} className="relative">
      <button
        ref={triggerRef}
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? dialogId : undefined}
        className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-xs text-neutral-300 hover:bg-neutral-800 focus:bg-neutral-800 focus:outline-none focus:ring-1 focus:ring-neutral-600"
      >
        <span
          aria-hidden="true"
          className={`flex h-6 w-6 shrink-0 items-center justify-center rounded-full text-[10px] font-semibold ${colorClass}`}
        >
          {initials}
        </span>
        <span className="flex-1 truncate">{me.name}</span>
        <svg
          aria-hidden="true"
          viewBox="0 0 12 12"
          className={`h-3 w-3 shrink-0 text-neutral-500 transition-transform ${open ? "rotate-180" : ""}`}
        >
          <path d="M2 8 L6 4 L10 8" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      </button>
      {open && (
        <div
          id={dialogId}
          role="dialog"
          aria-label="Account menu"
          className="absolute bottom-full left-0 z-50 mb-2 w-64 overflow-hidden rounded border border-neutral-800 bg-neutral-900 shadow-lg"
        >
          <div className="flex items-center gap-3 border-b border-neutral-800 px-3 py-3">
            <span
              aria-hidden="true"
              className={`flex h-10 w-10 shrink-0 items-center justify-center rounded-full text-sm font-semibold ${colorClass}`}
            >
              {initials}
            </span>
            <div className="min-w-0 flex-1">
              <div className="truncate text-sm font-medium text-neutral-100">{me.name}</div>
              {me.email && (
                <div className="truncate text-xs text-neutral-400" title={me.email}>
                  {me.email}
                </div>
              )}
            </div>
          </div>
          {groups.length > 0 && (
            <div className="flex flex-wrap gap-1 border-b border-neutral-800 px-3 py-2">
              {groups.map((g) => (
                <span
                  key={g}
                  className="rounded bg-neutral-800 px-2 py-0.5 text-xs text-neutral-300"
                >
                  {g}
                </span>
              ))}
            </div>
          )}
          {me.isAdmin && (
            <div className="border-b border-neutral-800 px-3 py-2">
              <span className="rounded bg-emerald-900/60 px-2 py-0.5 text-xs font-medium text-emerald-300">
                Admin
              </span>
            </div>
          )}
          <a
            href="/outpost.goauthentik.io/sign_out"
            className="block px-3 py-2 text-sm text-neutral-200 hover:bg-neutral-800 focus:bg-neutral-800 focus:outline-none"
          >
            Sign out
          </a>
        </div>
      )}
    </div>
  );
}
