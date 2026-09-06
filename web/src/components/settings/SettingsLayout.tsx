import { useCallback, useEffect, useRef, useState } from "react";

export interface SettingsCategory {
  id: string;
  label: string;
}

interface SettingsLayoutProps {
  categories: SettingsCategory[];
  children: React.ReactNode;
}

export function SettingsLayout({ categories, children }: SettingsLayoutProps) {
  const [activeId, setActiveId] = useState(categories[0]?.id ?? "");
  const containerRef = useRef<HTMLDivElement>(null);
  const observerRef = useRef<IntersectionObserver | null>(null);

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    observerRef.current?.disconnect();

    observerRef.current = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting) {
            const sectionId = entry.target.getAttribute("data-settings-section");
            if (sectionId) setActiveId(sectionId);
          }
        }
      },
      { rootMargin: "-20% 0px -60% 0px", threshold: 0 }
    );

    const sections = container.querySelectorAll("[data-settings-section]");
    for (const el of sections) {
      observerRef.current.observe(el);
    }

    return () => {
      observerRef.current?.disconnect();
    };
  }, [categories]);

  const scrollTo = useCallback((id: string) => {
    const el = containerRef.current?.querySelector(`#${CSS.escape(id)}`);
    if (el) {
      el.scrollIntoView({ behavior: "smooth", block: "start" });
      setActiveId(id);
    }
  }, []);

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
      <div ref={containerRef} className="min-w-0 flex-1 max-w-6xl">
        {children}
      </div>
    </div>
  );
}
