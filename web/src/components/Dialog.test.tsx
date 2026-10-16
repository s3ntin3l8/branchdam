import { useRef, useState } from "react";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import Dialog from "./Dialog";

function Harness({
  dismissible,
  onClose = vi.fn(),
  withInitialFocus = false,
}: {
  dismissible?: boolean;
  onClose?: () => void;
  withInitialFocus?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const secondRef = useRef<HTMLInputElement>(null);
  const close = () => {
    onClose();
    setOpen(false);
  };
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open
      </button>
      {open && (
        <Dialog
          title="Edit thing"
          onClose={close}
          dismissible={dismissible}
          initialFocusRef={withInitialFocus ? secondRef : undefined}
        >
          <input aria-label="First" />
          <input aria-label="Second" ref={secondRef} />
          <button type="button" onClick={close}>
            Done
          </button>
        </Dialog>
      )}
    </>
  );
}

describe("Dialog", () => {
  it("is a modal dialog labelled by its title", async () => {
    render(<Harness />);
    await userEvent.click(screen.getByRole("button", { name: "Open" }));

    const dialog = screen.getByRole("dialog", { name: "Edit thing" });
    expect(dialog).toHaveAttribute("aria-modal", "true");
  });

  it("moves focus to the first focusable element, or to initialFocusRef", async () => {
    const { unmount } = render(<Harness />);
    await userEvent.click(screen.getByRole("button", { name: "Open" }));
    expect(screen.getByRole("textbox", { name: "First" })).toHaveFocus();
    unmount();

    render(<Harness withInitialFocus />);
    await userEvent.click(screen.getByRole("button", { name: "Open" }));
    expect(screen.getByRole("textbox", { name: "Second" })).toHaveFocus();
  });

  it("traps Tab and Shift+Tab inside the dialog", async () => {
    render(<Harness />);
    await userEvent.click(screen.getByRole("button", { name: "Open" }));

    const first = screen.getByRole("textbox", { name: "First" });
    const done = screen.getByRole("button", { name: "Done" });

    await userEvent.tab();
    await userEvent.tab();
    expect(done).toHaveFocus();
    await userEvent.tab();
    expect(first).toHaveFocus();
    await userEvent.tab({ shift: true });
    expect(done).toHaveFocus();
  });

  it("closes on Escape and on a backdrop click, and returns focus to the opener", async () => {
    const onClose = vi.fn();
    render(<Harness onClose={onClose} />);
    const opener = screen.getByRole("button", { name: "Open" });

    await userEvent.click(opener);
    await userEvent.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(opener).toHaveFocus();

    await userEvent.click(opener);
    // The backdrop is the dialog panel's parent.
    await userEvent.click(screen.getByRole("dialog").parentElement as HTMLElement);
    expect(onClose).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("ignores a click inside the panel", async () => {
    const onClose = vi.fn();
    render(<Harness onClose={onClose} />);
    await userEvent.click(screen.getByRole("button", { name: "Open" }));
    await userEvent.click(screen.getByRole("dialog"));
    expect(onClose).not.toHaveBeenCalled();
  });

  it("stays open when a drag starts in the panel and is released on the backdrop", async () => {
    const onClose = vi.fn();
    render(<Harness onClose={onClose} />);
    await userEvent.click(screen.getByRole("button", { name: "Open" }));
    const overlay = screen.getByRole("dialog").parentElement as HTMLElement;
    const input = screen.getByLabelText("First");

    // What a browser sends for a text selection dragged out of an input
    // and released over the backdrop: mousedown on the input, mouseup on
    // the backdrop, then a click on their common ancestor -- the overlay.
    fireEvent.mouseDown(input);
    fireEvent.mouseUp(overlay);
    fireEvent.click(overlay);
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    // A press that starts and ends on the backdrop still closes it.
    fireEvent.mouseDown(overlay);
    fireEvent.mouseUp(overlay);
    fireEvent.click(overlay);
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("ignores Escape and the backdrop when not dismissible", async () => {
    const onClose = vi.fn();
    render(<Harness onClose={onClose} dismissible={false} />);
    await userEvent.click(screen.getByRole("button", { name: "Open" }));

    await userEvent.keyboard("{Escape}");
    await userEvent.click(screen.getByRole("dialog").parentElement as HTMLElement);
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("only the topmost of two stacked dialogs reacts to Escape", async () => {
    const outerClose = vi.fn();
    const innerClose = vi.fn();
    function Stacked() {
      const [inner, setInner] = useState(true);
      return (
        <>
          <Dialog title="Outer" onClose={outerClose}>
            <button type="button">Outer button</button>
          </Dialog>
          {inner && (
            <Dialog
              title="Inner"
              onClose={() => {
                innerClose();
                setInner(false);
              }}
            >
              <button type="button">Inner button</button>
            </Dialog>
          )}
        </>
      );
    }
    render(<Stacked />);
    expect(screen.getByRole("button", { name: "Inner button" })).toHaveFocus();

    await userEvent.keyboard("{Escape}");
    expect(innerClose).toHaveBeenCalledTimes(1);
    expect(outerClose).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog", { name: "Inner" })).not.toBeInTheDocument();

    await userEvent.keyboard("{Escape}");
    expect(outerClose).toHaveBeenCalledTimes(1);
  });

  it("gives each instance its own title id", () => {
    render(
      <>
        <Dialog title="One">
          <p>a</p>
        </Dialog>
        <Dialog title="Two">
          <p>b</p>
        </Dialog>
      </>,
    );
    const ids = screen.getAllByRole("dialog").map((d) => d.getAttribute("aria-labelledby"));
    expect(new Set(ids).size).toBe(2);
    expect(screen.getByRole("dialog", { name: "One" })).toBeInTheDocument();
    expect(screen.getByRole("dialog", { name: "Two" })).toBeInTheDocument();
  });
});
