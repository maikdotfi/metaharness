// Package sqlfs embeds the goose migration files so the server can run
// migrations on startup without needing the migration directory on disk.
package sqlfs

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
