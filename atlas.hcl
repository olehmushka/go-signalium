// Atlas configuration for go-signalium.
//
// Versioned SQL migrations live under ./migrations and are applied in order
// against the `signalium` schema. The same
// directory is embedded into the binary via embed.FS so the worker process
// can auto-run pending migrations on startup, guarded by a pg_advisory_lock
// to prevent multi-replica races (see docs/persistence.md).
//
// Usage:
//   atlas migrate diff <name> --env local
//   atlas migrate apply           --env local
//   atlas migrate status          --env local
//   atlas migrate hash            --env local

env "local" {
  src = "file://migrations"
  url = getenv("SIGNALIUM_DB_URL") != "" ? getenv("SIGNALIUM_DB_URL") : "postgres://myuser:mypassword@localhost:5432/mydb?sslmode=disable"
  dev = "docker://postgis/postgis/17-3.6-alpine/dev?search_path=signalium"
  schemas = ["signalium", "atlas_schema_revisions"]

  migration {
    dir    = "file://migrations"
    format = atlas
    # Keeps Atlas tracking data in public, separate from your app data
    revisions_schema = "atlas_schema_revisions"
  }

  format {
    migrate {
      diff = "{{ sql . \"  \" }}"
    }
  }
}

env "ci" {
  src = "file://migrations"
  url = getenv("SIGNALIUM_DB_URL")
  dev = "docker://postgis/postgis/17-3.6-alpine/dev?search_path=signalium"

  migration {
    dir    = "file://migrations"
    format = atlas
  }
}
