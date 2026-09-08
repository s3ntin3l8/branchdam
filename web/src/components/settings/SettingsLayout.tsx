import { useCallback, useEffect, useRef, useState } from "react";

export interface SettingsCategory {
  id: string;
  label: string;
}

interface SettingsLayoutProps {
  categories: SettingsCategory[];
  children: React.ReactNode;
}

// Quiet window after the last scroll event before releasing programmatic lock
const SCROLL_SETTLE_MS = 150;
// Pixel distance from bottom of scrollable container to treat as bottomed out
const BOTTOM_THRESHOLD_PX = 24;

function getScrollParent(element: HTMLElement | null): HTMLElement | Window | null {
  if (typeof window === "undefined") return null;
  let el: HTMLElement | null = element?.parentElement ?? null;
  while (el) {
    if (window.getComputedStyle) {
      const overflowY = window.getComputedStyle(el).overflowY;
      if (overflowY === "auto" || overflowY === "scroll") {
        return el;
      }
    }
    el = el.parentElement;
  }
  return window;
}

function isScrolledToBottom(scrollParent: HTMLElement | Window | null): boolean {
  if (scrollParent instanceof HTMLElement) {
    return scrollParent.scrollHeight - scrollParent.scrollTop - scrollParent.clientHeight <= BOTTOM_THRESHOLD_PX;
  }
  if (scrollParent === window && typeof document !== "undefined") {
    return window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - BOTTOM_THRESHOLD_PX;
  }
  return false;
}

export function SettingsLayout({ categories, children }: SettingsLayoutProps) {
  const [activeId, setActiveId] = useState(categories[0]?.id ?? "");
  const containerRef = useRef<HTMLDivElement>(null);
  const observerRef = useRef<IntersectionObserver | null>(null);
  const isProgrammaticScrollRef = useRef(false);
  const scrollTimeoutRef = useRef<number | null>(null);

  const clearScrollTimeout = useCallback(() => {
    if (scrollTimeoutRef.current !== null) {
      window.clearTimeout(scrollTimeoutRef.current);
      scrollTimeoutRef.current = null;
    }
  }, []);

  const resetScrollSettlingTimer = useCallback(() => {
    clearScrollTimeout();
    scrollTimeoutRef.current = window.setTimeout(() => {
      isProgrammaticScrollRef.current = false;
      scrollTimeoutRef.current = null;
    }, SCROLL_SETTLE_MS);
  }, [clearScrollTimeout]);

  const unlockProgrammaticScroll = useCallback(() => {
    if (isProgrammaticScrollRef.current) {
      clearScrollTimeout();
      isProgrammaticScrollRef.current = false;
    }
  }, [clearScrollTimeout]);

  const scrollTo = useCallback((id: string) => {
    const el = containerRef.current?.querySelector(`#${CSS.escape(id)}`);
    if (el) {
      isProgrammaticScrollRef.current = true;
      setActiveId(id);
      if (typeof window !== "undefined" && window.history?.replaceState) {
        window.history.replaceState(null, "", `#${id}`);
      }
      if (typeof el.scrollIntoView === "function") {
        el.scrollIntoView({ behavior: "smooth", block: "start" });
      }
      resetScrollSettlingTimer();
    }
  }, [resetScrollSettlingTimer]);

  useEffect(() => {
    const hash = window.location.hash.replace(/^#/, "");
    if (hash && categories.some((c) => c.id === hash)) {
      scrollTo(hash);
    }
  }, [categories, scrollTo]);

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    observerRef.current?.disconnect();

    if (typeof IntersectionObserver !== "undefined") {
      observerRef.current = new IntersectionObserver(
        (entries) => {
          if (isProgrammaticScrollRef.current) return;
          for (const entry of entries) {
            if (entry.isIntersecting) {
              const sectionId = entry.target.getAttribute("data-settings-section");
              if (sectionId) setActiveId(sectionId);
            }
          }
        },
        { rootMargin: "0px 0px -65% 0px", threshold: 0 }
      );

      const sections = container.querySelectorAll("[data-settings-section]");
      for (const el of sections) {
        observerRef.current.observe(el);
      }
    }

    const scrollParent = getScrollParent(container);
    const handleScroll = () => {
      if (isProgrammaticScrollRef.current) {
        // Keep the lock held while smooth-scroll events are still arriving
        resetScrollSettlingTimer();
        return;
      }
      if (categories.length === 0) return;

      if (isScrolledToBottom(scrollParent)) {
        setActiveId(categories[categories.length - 1].id);
      }
    };

    const handleUserInput = () => {
      unlockProgrammaticScroll();
    };

    if (scrollParent && typeof scrollParent.addEventListener === "function") {
      scrollParent.addEventListener("scroll", handleScroll, { passive: true });
    }
    if (typeof window !== "undefined" && typeof window.addEventListener === "function") {
      window.addEventListener("wheel", handleUserInput, { passive: true });
      window.addEventListener("touchstart", handleUserInput, { passive: true });
      window.addEventListener("keydown", handleUserInput, { passive: true });
    }

    return () => {
      observerRef.current?.disconnect();
      if (scrollParent && typeof scrollParent.removeEventListener === "function") {
        scrollParent.removeEventListener("scroll", handleScroll);
      }
      if (typeof window !== "undefined" && typeof window.removeEventListener === "function") {
        window.removeEventListener("wheel", handleUserInput);
        window.removeEventListener("touchstart", handleUserInput);
        window.removeEventListener("keydown", handleUserInput);
      }
      clearScrollTimeout();
      isProgrammaticScrollRef.current = false;
    };
  }, [categories, resetScrollSettlingTimer, unlockProgrammaticScroll, clearScrollTimeout]);

  return (
    <div className="flex gap-6">
      <nav className="hidden w-44 shrink-0 md:block">
        <div className="sticky top-6 space-y-0.5">
          {categories.map((cat) => (
            <a
              key={cat.id}
              href={`#${cat.id}`}
              onClick={(e) => {
                e.preventDefault();
                scrollTo(cat.id);
              }}
              className={`block rounded px-3 py-1.5 text-sm font-medium ${
                activeId === cat.id
                  ? "bg-neutral-800 text-neutral-100"
                  : "text-neutral-400 hover:bg-neutral-900 hover:text-neutral-200"
              }`}
            >
              {cat.label}
            </a>
          ))}
        </div>
      </nav>
      <div ref={containerRef} className="min-w-0 flex-1 max-w-6xl pb-[40vh]">
        {children}
      </div>
    </div>
  );
}
