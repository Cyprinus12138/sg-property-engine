# migrations

Single source of truth for the engine's Postgres schema. `sgpe serve` and
`sgpe migrate` apply them at boot via `db.RunMigrations`; the files are also
usable with the standalone `migrate` CLI.

```
000001_init.up.sql    — PostGIS extension + every engine table and index.
000001_init.down.sql  — drops everything the up created (dev reset only).
```

## Authoring a new migration

```
migrate create -ext sql -dir migrations -seq <short_name>
```

Edit both files. Test the down: `down → up` must be a no-op. Never edit a
migration that has shipped; add a new one. Numbers must be globally unique
and strictly increasing — golang-migrate refuses to run (and can leave the
schema `dirty`) when two files share a number, and git merges migrations by
filename with no textual conflict, so re-check the highest number after
every rebase.
