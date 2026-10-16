import { useEffect, useId, useRef, type ReactNode, type RefObject } from "react";

// Dialog is the SPA's one modal primitive. Every modal -- ConfirmDialog,
// NodePickerModal, the users/pairing/audit modals, the unsaved-changes
// prompt, the image lightbox -- renders through it, so they all share the
// same contract:
//
//   - role="dialog" + aria-modal, labelled by its title (aria-labelledby).
//   - Focus moves into the dialog when it opens (initialFocusRef, else the
//     first focusable element, else the panel itself) and goes back to
//     whatever had it before when the dialog closes.
//   - Tab / Shift+Tab are trapped inside the panel.
//   - Escape and a backdrop click call onClose, unless `dismissible` is
//     false. Use that for dialogs that must not vanish by accident (a
//     one-time secret, a request in flight); they then close only through
//     their own buttons.
//
// Open dialogs form a stack, so with one dialog on top of another (the node
// picker opened from the manual-edge modal) only the topmost reacts to
// Escape and Tab.
export interface DialogProps {
  title: ReactNode;
  children: ReactNode;
  onClose?: () => void;
  // false: Escape and backdrop clicks are ignored. Default true.
  dismissible?: boolean;
  // Explicit id for the title element; generated when omitted.
  titleId?: string;
  initialFocusRef?: RefObject<HTMLElement | null>;
  // Classes for the panel; replaces the default card styling.
  panelClassName?: string;
  // Extra classes for the full-screen backdrop (e.g. z-index, darkness).
  overlayClassName?: string;
  titleClassName?: string;
  // Rendered beside the title (e.g. a close button). The title and this
  // share a row styled by headerClassName.
  headerAction?: ReactNode;
  headerClassName?: string;
}

const FOCUSABLE =
  'a[href], button:not([disabled]), textarea:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), [tabindex]:not([tabindex="-1"])';

// Module-level stack of open dialogs, newest last.
const openDialogs: symbol[] = [];

function focusableIn(el: HTMLElement): HTMLElement[] {
  return Array.from(el.querySelectorAll<HTMLElement>(FOCUSABLE));
}

const dialogPanelClass =
  "w-full max-w-md rounded-lg border border-neutral-800 bg-neutral-900 p-6 shadow-xl max-h-[90vh] overflow-y-auto";

export default function Dialog({
  title,
  children,
  onClose,
  dismissible = true,
  titleId,
  initialFocusRef,
  panelClassName = dialogPanelClass,
  overlayClassName = "z-50 bg-black/70",
  titleClassName = "mb-4 text-base font-semibold text-neutral-100",
  headerAction,
  headerClassName = "mb-4 flex items-center justify-between gap-4",
}: DialogProps) {
  const generatedId = useId();
  const labelId = titleId ?? `dialog-title-${generatedId}`;
  const panelRef = useRef<HTMLDivElement>(null);
  // Whether the current press started on the backdrop itself. A click is
  // dispatched on the common ancestor of the mousedown and mouseup
  // targets, so a text selection dragged from inside the panel and
  // released on the backdrop arrives as a click on the overlay; only a
  // press that both starts and ends on the backdrop closes the dialog.
  const pressStartedOnOverlay = useRef(false);

  // Latest props for the document listener, without re-subscribing (and so
  // without re-running the focus-on-open effect) on every render.
  const onCloseRef = useRef(onClose);
  const dismissibleRef = useRef(dismissible);
  useEffect(() => {
    onCloseRef.current = onClose;
    dismissibleRef.current = dismissible;
  });

  useEffect(() => {
    const token = Symbol("dialog");
    openDialogs.push(token);
    const previouslyFocused = document.activeElement as HTMLElement | null;
    const panel = panelRef.current;

    if (panel) {
      const target = initialFocusRef?.current ?? focusableIn(panel)[0] ?? panel;
      target.focus();
    }

    const onKeyDown = (e: KeyboardEvent) => {
      if (openDialogs[openDialogs.length - 1] !== token || !panel) return;
      if (e.key === "Escape") {
        if (dismissibleRef.current && onCloseRef.current) {
          e.preventDefault();
          onCloseRef.current();
        }
        return;
      }
      if (e.key !== "Tab") return;
      const items = focusableIn(panel);
      if (items.length === 0) {
        e.preventDefault();
        panel.focus();
        return;
      }
      const first = items[0];
      const last = items[items.length - 1];
      const active = document.activeElement;
      if (!panel.contains(active)) {
        e.preventDefault();
        (e.shiftKey ? last : first).focus();
      } else if (e.shiftKey && (active === first || active === panel)) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && active === last) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", onKeyDown);

    return () => {
      document.removeEventListener("keydown", onKeyDown);
      const i = openDialogs.indexOf(token);
      if (i !== -1) openDialogs.splice(i, 1);
      if (previouslyFocused && previouslyFocused.isConnected && typeof previouslyFocused.focus === "function") {
        previouslyFocused.focus();
      }
    };
    // initialFocusRef is read once, on open.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div
      className={`fixed inset-0 flex items-center justify-center p-4 ${overlayClassName}`}
      onMouseDown={(e) => {
        pressStartedOnOverlay.current = e.target === e.currentTarget;
      }}
      onClick={(e) => {
        const startedOnOverlay = pressStartedOnOverlay.current;
        pressStartedOnOverlay.current = false;
        if (startedOnOverlay && e.target === e.currentTarget && dismissible && onClose) onClose();
      }}
    >
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={labelId}
        tabIndex={-1}
        className={`focus:outline-none ${panelClassName}`}
      >
        {headerAction ? (
          <div className={headerClassName}>
            <h2 id={labelId} className={titleClassName}>
              {title}
            </h2>
            {headerAction}
          </div>
        ) : (
          <h2 id={labelId} className={titleClassName}>
            {title}
          </h2>
        )}
        {children}
      </div>
    </div>
  );
}
