import { useCallback, useEffect, useRef, useState } from "react";

export interface SettingsCategory {
  id: string;
  label: string;
}

interface SettingsLayoutProps {
  categories: SettingsCategory[];
  children: React.ReactNode;
}

function getScrollParent(element: HTMLElement | null): HTMLElement | Window {
  let el: HTMLElement | null = element?.parentElement ?? null;
  while (el) {
    if (typeof window !== "undefined" && window.getComputedStyle) {
      const overflowY = window.getComputedStyle(el).overflowY;
      if (overflowY === "auto" || overflowY === "scroll") {
        return el;
      }
    }
    el = el.parentElement;
  }
  return typeof window !== "undefined" ? window : (null as unknown as Window);
}

export function SettingsLayout({ categories, children }: SettingsLayoutProps) {
  const [activeId, setActiveId] = useState(categories[0]?.id ?? "");
  const containerRef = useRef<HTMLDivElement>(null);
  const observerRef = useRef<IntersectionObserver | null>(null);
  const isProgrammaticScrollRef = useRef(false);
  const scrollTimeoutRef = useRef<number | null>(null);

  const scrollTo = useCallback((id: string) => {
    const el = containerRef.current?.querySelector(`#${CSS.escape(id)}`);
    if (el) {
      if (scrollTimeoutRef.current !== null) {
        window.clearTimeout(scrollTimeoutRef.current);
      }
      isProgrammaticScrollRef.current = true;
      setActiveId(id);
      if (typeof el.scrollIntoView === "function") {
        el.scrollIntoView({ behavior: "smooth", block: "start" });
      }
      scrollTimeoutRef.current = window.setTimeout(() => {
        isProgrammaticScrollRef.current = false;
        scrollTimeoutRef.current = null;
      }, 800);
    }
  }, []);

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
      if (isProgrammaticScrollRef.current || categories.length === 0) return;

      let isAtBottom = false;
      if (scrollParent instanceof HTMLElement) {
        isAtBottom = scrollParent.scrollHeight - scrollParent.scrollTop - scrollParent.clientHeight <= 24;
      } else if (typeof window !== "undefined" && typeof document !== "undefined") {
        isAtBottom = window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 24;
      }

      if (isAtBottom) {
        setActiveId(categories[categories.length - 1].id);
      }
    };

    if (scrollParent && typeof scrollParent.addEventListener === "function") {
      scrollParent.addEventListener("scroll", handleScroll, { passive: true });
    }

    return () => {
      observerRef.current?.disconnect();
      if (scrollParent && typeof scrollParent.removeEventListener === "function") {
        scrollParent.removeEventListener("scroll", handleScroll);
      }
      if (scrollTimeoutRef.current !== null) {
        window.clearTimeout(scrollTimeoutRef.current);
      }
    };
  }, [categories]);

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
