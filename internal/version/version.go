// Package version holds the version strings reported to clients.
package version

const (
	// Capivara is the version of this project.
	Capivara = "0.1.0"

	// PGCompat is reported as server_version. Drivers parse it to decide
	// which features to use (pgjdbc refuses servers older than 8.2, psql
	// tailors its catalog queries to it), so it has to look like a real
	// PostgreSQL version rather than our own.
	PGCompat = "16.0"
)

// Full is what version() returns.
const Full = "CapivaraDB " + Capivara + " (PostgreSQL " + PGCompat + " wire-compatible)"
