package migration

// BaselineFiles names the frozen, generated baseline every compacted dialect
// was cut to -- postgres in cleat#2059, MySQL in cleat#2433, SQL Server in
// cleat#2434. docs/contributor/migrations.md names them once, and this is
// the single Go-level source of truth for that name.
//
// Two consumers need this exact list to agree with each other, and cleat#2472
// is the record of what happens when they do not: engine/routine_definition_
// drift_test.go's supersession guard treats these files as the whole baseline
// population, and scripts/gen-mssql-baseline/verify.go treats them as the
// files the generator owns and verifies byte-for-byte. Each used to carry its
// own copy of this list -- same three names, no assertion that they agreed --
// so a dialect's baseline growing a fourth file would update one consumer and
// leave the other silently checking a population that no longer existed.
var BaselineFiles = []string{"001_schema.sql", "002_defaults.sql", "003_procedures.sql"}
