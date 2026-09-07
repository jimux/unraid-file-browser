package api

import (
	"testing"

	"unraid-filebrowser/internal/archive"
	"unraid-filebrowser/internal/fsops"
	"unraid-filebrowser/internal/index"
)

// The api package declares its dependencies as consumer interfaces, so nothing
// forces the concrete subsystems to keep matching them until main wires them up
// — at which point a signature drift is an integration-day surprise. These
// assertions move that failure to `go test ./internal/api/...`.
//
// They are the *only* place api touches the concrete packages; the handlers
// themselves stay decoupled.
var (
	_ FileSystem   = (*fsops.FS)(nil)
	_ ArchiveFS    = (*archive.Resolver)(nil)
	_ IndexService = (*index.Service)(nil)
)

func TestConcreteSubsystemsSatisfyTheRouterContract(t *testing.T) {
	// The assertions above are compile-time; this test exists so the intent is
	// visible in `go test -v` output.
	t.Log("fsops.FS, archive.Resolver and index.Service satisfy api's interfaces")
}
