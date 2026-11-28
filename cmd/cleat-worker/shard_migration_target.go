package main

import "database/sql"

// shardMigrationTarget is one shard's database as the migration step needs it: the name
// the shards file gives it (so a failure can say which database), its pool, and the
// schema its tables live in. cleat#3718.
type shardMigrationTarget struct {
	name   string
	db     *sql.DB
	schema string
}
