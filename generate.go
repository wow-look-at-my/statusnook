package main

// SQL is not hand-written: the queries live in sql/*.sql and sqlc compiles
// them into typed Go under internal/sqlcgen. A column rename that breaks a
// query is then a build error rather than a scan failure at runtime, and the
// generated files carry the standard "Code generated ... DO NOT EDIT." marker
// so tooling treats them as the artifacts they are.
//
//go:generate go tool sqlc generate
