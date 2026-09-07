import { useState } from "react";

// PendingResetsPanel is a stub shipped with PR #409 (password-reset
// infrastructure). The full admin-UI implementation lands in PR #408
// (admin user management), which will mount this on /admin/users
// alongside the UsersTable and the LoginAuditTail. For now, the
// component is exported so the LoginPage can reach for it as a
// forward reference, and so #408 has a defined target shape to
// replace.
//
// When the user-facing self-service flow is triggered, the
// response is intentionally empty (200 OK, no token, no user
// info) -- an enumeration defense. The token surfaces in two
// places: (1) the slog.WARN log line in the server, and (2) the
// admin-UI pending-resets panel (this component, once #408 lands).
//
// The component is intentionally minimal here: a placeholder so
// the PR compiles and the route shape is reserved. The real
// implementation will copy a token to the clipboard, render the
// expires_in duration, and revoke via DELETE /api/v1/admin/users/{id}/
// resets/{tokenId}.
export interface PendingReset {
  id: number;
  userId: number;
  createdAt: number;
  expiresAt: number;
  // Token plaintext is NEVER exposed via the API -- the admin
  // retrieves it from the slog.WARN log line, not from this panel.
  // The "copy token" UX in #408 will surface a clickable link to
  // the operator's journalctl/tail session instead.
}

export default function PendingResetsPanel() {
  const [hidden] = useState(true);
  if (hidden) {
    return (
      <div
        data-testid="pending-resets-panel-stub"
        className="rounded border border-dashed border-neutral-700 bg-neutral-900/40 p-3 text-xs text-neutral-500"
      >
        <p className="font-medium text-neutral-300">Pending password resets</p>
        <p className="mt-1">
          The full pending-resets panel ships with the admin user-management UI
          (PR #408). For now, retrieve tokens from the server log
          (slog.WARN, plaintext_token=...).
        </p>
      </div>
    );
  }
  return null;
}
