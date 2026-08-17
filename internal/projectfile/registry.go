package projectfile

import (
	"strings"
	"sync"
)

var (
	mu       sync.RWMutex
	registry = make(map[string]Parser)
)

// Register registers a project file parser for its reported extensions.
// Ext extensions are normalized to lowercase without leading dot.
func Register(p Parser) {
	mu.Lock()
	defer mu.Unlock()

	if p == nil {
		panic("projectfile: Register parser is nil")
	}

	for _, ext := range p.Extensions() {
		norm := normalizeExt(ext)
		if norm == "" {
			continue
		}
		registry[norm] = p
	}
}

// GetParser returns the registered Parser for the given file extension or file name.
func GetParser(filenameOrExt string) (Parser, bool) {
	mu.RLock()
	defer mu.RUnlock()

	norm := normalizeFilenameOrExt(filenameOrExt)
	p, ok := registry[norm]
	return p, ok
}

// SupportedExtensions returns a slice of all registered file extensions.
func SupportedExtensions() []string {
	mu.RLock()
	defer mu.RUnlock()

	exts := make([]string, 0, len(registry))
	for ext := range registry {
		exts = append(exts, ext)
	}
	return exts
}

// ClearRegistry resets the global registry. Used primarily in unit tests.
func ClearRegistry() {
	mu.Lock()
	defer mu.Unlock()
	registry = make(map[string]Parser)
}

func normalizeExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	ext = strings.TrimPrefix(ext, ".")
	return ext
}

func normalizeFilenameOrExt(filename string) string {
	filename = strings.ToLower(strings.TrimSpace(filename))
	// Check compound extension first (e.g., .dam.json)
	if strings.HasSuffix(filename, ".dam.json") {
		return "dam.json"
	}
	if strings.HasSuffix(filename, ".drp") {
		return "drp"
	}
	if strings.HasSuffix(filename, ".fcpxml") {
		return "fcpxml"
	}
	if strings.HasSuffix(filename, ".edl") {
		return "edl"
	}
	idx := strings.LastIndex(filename, ".")
	if idx != -1 {
		return filename[idx+1:]
	}
	return filename
}
