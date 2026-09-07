// Package index — crawling and search over the array.
//
// This file holds the *crawl policy*: the judgment calls about what is worth
// indexing on this particular server. The mechanical crawler lives elsewhere
// in this package and calls these functions on every directory and file it
// visits. NOTE TO AGENTS: build the crawler around these exact signatures and
// do not rewrite their bodies — the policy is owner-tuned.
package index

import "strings"

// SkipDir reports whether the crawler should skip an entire directory
// subtree. `name` is the bare directory name (not the full path); `path` is
// the absolute path. Skipping here prunes both the metadata index and content
// indexing, so this is the main lever against index bloat and wasted IO.
//
// Policy: a fixed blocklist of universally-worthless names (VCS internals,
// dependency dirs, NAS/OS trash) plus the .Trash-<uid> pattern. Only what is
// listed gets pruned; `path` is available for server-specific prefix rules.
func SkipDir(name, path string) bool {
	switch name {
	case ".git", ".svn", "node_modules",
		"__pycache__", ".venv", ".cache",
		"lost+found", ".Recycle.Bin",
		"System Volume Information",
		"$RECYCLE.BIN":
		return true
	}
	return strings.HasPrefix(name, ".Trash-")
}

// DefaultContentExtensions returns the extension allowlist (lowercase, no
// dot) used for content indexing when no config exists yet — the out-of-box
// definition of "text-like enough to be worth full-text indexing".
//
// Policy: docs/text, code, config, and logs. Every extension here is read
// end-to-end (up to the size cap) during content crawls; over-cap files are
// skipped entirely, so runaway logs cost nothing.
func DefaultContentExtensions() []string {
	return []string{
		// docs/text
		"txt", "md", "rst", "csv", "json", "yaml", "yml", "xml", "html",
		// code
		"go", "py", "js", "ts", "sh", "php", "c", "h", "cpp", "rs", "java", "sql", "css",
		// config
		"conf", "cfg", "ini", "env", "toml", "properties",
		// logs
		"log",
	}
}
