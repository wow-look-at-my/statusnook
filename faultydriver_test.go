package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"

	"github.com/mattn/go-sqlite3"
)

// A sqlite3 driver that counts every statement the app runs and fails the one
// the test names. Handlers open a transaction, run several queries and commit,
// with a branch after each -- and those branches decide whether a failure ends
// as a 500 or as a half-written database. Killing the connection outright, the
// way TestHandlersFailCleanlyWhenTheDatabaseIsGone does, only ever reaches the
// first of them.
var faultAt atomic.Int64

var errInjectedFault = errors.New("injected database fault")

// failAtStatement arms the driver to fail the nth statement from now, counting
// prepares, executions and commits alike. n <= 0 disarms it.
func failAtStatement(n int64) {
	faultAt.Store(n)
}

// Returns errInjectedFault exactly once, on the nth call after arming.
func nextFault() error {
	remaining := faultAt.Add(-1)
	if remaining != 0 {
		return nil
	}

	return errInjectedFault
}

// Whether the armed fault was reached. Below one means the counter ran past the
// statement it was aimed at, so the request got that far; above it means the
// request finished with fewer statements than that and nothing was injected.
func faultFired() bool {
	return faultAt.Load() < 1
}

func init() {
	sql.Register("sqlite3-faulty", &faultyDriver{})
}

type faultyDriver struct{}

func (d *faultyDriver) Open(name string) (driver.Conn, error) {
	conn, err := (&sqlite3.SQLiteDriver{}).Open(name)
	if err != nil {
		return nil, err
	}

	return &faultyConn{Conn: conn}, nil
}

// Only Prepare and Begin are wrapped, and neither ExecerContext nor
// QueryerContext is promoted -- embedding an interface does not satisfy them --
// so database/sql routes every query through the statements below.
type faultyConn struct {
	driver.Conn
}

func (c *faultyConn) Prepare(query string) (driver.Stmt, error) {
	if err := nextFault(); err != nil {
		return nil, err
	}

	stmt, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}

	return &faultyStmt{Stmt: stmt}, nil
}

func (c *faultyConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := nextFault(); err != nil {
		return nil, err
	}

	preparer, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Conn.Prepare(query)
	}

	stmt, err := preparer.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}

	return &faultyStmt{Stmt: stmt}, nil
}

func (c *faultyConn) Begin() (driver.Tx, error) {
	if err := nextFault(); err != nil {
		return nil, err
	}

	return c.Conn.Begin()
}

func (c *faultyConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := nextFault(); err != nil {
		return nil, err
	}

	beginner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return c.Conn.Begin()
	}

	return beginner.BeginTx(ctx, opts)
}

type faultyStmt struct {
	driver.Stmt
}

func (s *faultyStmt) Exec(args []driver.Value) (driver.Result, error) {
	if err := nextFault(); err != nil {
		return nil, err
	}

	return s.Stmt.Exec(args)
}

func (s *faultyStmt) Query(args []driver.Value) (driver.Rows, error) {
	if err := nextFault(); err != nil {
		return nil, err
	}

	return s.Stmt.Query(args)
}

func (s *faultyStmt) ExecContext(
	ctx context.Context,
	args []driver.NamedValue,
) (driver.Result, error) {
	if err := nextFault(); err != nil {
		return nil, err
	}

	execer, ok := s.Stmt.(driver.StmtExecContext)
	if !ok {
		return nil, driver.ErrSkip
	}

	return execer.ExecContext(ctx, args)
}

func (s *faultyStmt) QueryContext(
	ctx context.Context,
	args []driver.NamedValue,
) (driver.Rows, error) {
	if err := nextFault(); err != nil {
		return nil, err
	}

	queryer, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, driver.ErrSkip
	}

	return queryer.QueryContext(ctx, args)
}

// Swaps the app's two handles for ones on the faulting driver, pointed at the
// same file initDB already created.
func withFaultyDB(app *testApp) {
	app.t.Helper()

	previousDB, previousRWDB := db, rwDB

	read, err := sql.Open("sqlite3-faulty", dbDSN(false))
	if err != nil {
		app.t.Fatalf("withFaultyDB.OpenRead: %s", err)
	}

	write, err := sql.Open("sqlite3-faulty", dbDSN(true))
	if err != nil {
		app.t.Fatalf("withFaultyDB.OpenWrite: %s", err)
	}
	write.SetMaxOpenConns(1)

	db, rwDB = read, write

	app.t.Cleanup(func() {
		failAtStatement(0)
		read.Close()
		write.Close()
		db, rwDB = previousDB, previousRWDB
	})
}
