// Package migrations exposes the versioned Atlas migration directory as an
// embed.FS. The db.Module mounts this FS as a memory:// dir for
// atlas migrate apply at boot, guarded by a Postgres advisory lock.
package migrations

import "embed"

// FS holds every committed .sql migration plus atlas.sum.
//
//go:embed *.sql atlas.sum
var FS embed.FS
