// Package web embeds the built SPA (web/dist) into the binary.
package web

import (
	"embed"
	"io/fs"
)

// dist holds the built SPA. The web/dist directory is gitignored and produced
// by the frontend build (vite, added in PR 10) or the Docker image build; for
// a bare `go build` without a frontend build, `make web-stub` (called by
// `make build`/`make test`/`make dev`, and by CI via .github/ci-prebuild.sh)
// stubs a placeholder web/dist/index.html.
//
//go:embed all:dist
var dist embed.FS

// Dist returns the built SPA filesystem rooted at the dist directory.
func Dist() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}
