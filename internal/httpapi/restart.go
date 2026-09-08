package httpapi

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"

	auditPkg "github.com/s3ntin3l8/branchdam/internal/audit"
)

// restartGraceDelay is how long handlePostRestart waits, after writing its
// response, before invoking the restart hook. httpServer.Shutdown
// (triggered indirectly by that hook, via the same stop() SIGTERM uses --
// see cmd/branchdam/main.go) blocks until every in-flight request finishes,
// including this one; firing the hook synchronously would make Shutdown
// wait on the very request that requested it, burning its own timeout
// budget for nothing. Firing it from a goroutine, after the handler has
// already returned and the response has had a moment to flush, avoids that
// self-wait entirely.
const restartGraceDelay = 250 * time.Millisecond

type PostRestartOutput struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

// handlePostRestart triggers a graceful process restart: the same
// requireSettingsAdmin gate as GET/PUT /api/v1/settings (so an agent
// principal holding only its API key can never bounce the process), then
// -- if this deployment wired one -- schedules Server.requestRestart after
// restartGraceDelay and returns success immediately. A nil
// Server.requestRestart (every existing test, and any Server built without
// Deps.RequestRestart) means this deployment has no restart hook wired at
// all, which is reported as 503 rather than silently doing nothing.
func (s *Server) handlePostRestart(ctx context.Context, _ *struct{}) (*PostRestartOutput, error) {
	if err := s.requireSettingsAdmin(ctx); err != nil {
		return nil, err
	}
	if s.requestRestart == nil {
		return nil, huma.Error503ServiceUnavailable("restart is not supported by this deployment")
	}

	s.log.Info("restart requested", "actor", principalName(ctx))

	if s.audit != nil {
		// Audit BEFORE scheduling the restart -- the deferred
		// goroutine may race with the process actually exiting,
		// and an audit row that lands after the binary is gone is
		// strictly less useful than one that lands before.
		if err := s.audit.WriteActorAudit(ctx, principalFromCtx(ctx), auditPkg.EventRestart, "system", "", nil); err != nil {
			s.log.Warn("audit: restart write failed", "err", err)
		}
	}

	requestRestart := s.requestRestart
	go func() {
		time.Sleep(restartGraceDelay)
		requestRestart()
	}()

	out := &PostRestartOutput{}
	out.Body.OK = true
	return out, nil
}
