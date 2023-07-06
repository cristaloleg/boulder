package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/go-sql-driver/mysql"
	"github.com/letsencrypt/borp"
)

// ErrDatabaseOp wraps an underlying err with a description of the operation
// that was being performed when the error occurred (insert, select, select
// one, exec, etc) and the table that the operation was being performed on.
type ErrDatabaseOp struct {
	Op    string
	Table string
	Err   error
}

// Error for an ErrDatabaseOp composes a message with context about the
// operation and table as well as the underlying Err's error message.
func (e ErrDatabaseOp) Error() string {
	// If there is a table, include it in the context
	if e.Table != "" {
		return fmt.Sprintf(
			"failed to %s %s: %s",
			e.Op,
			e.Table,
			e.Err)
	}
	return fmt.Sprintf(
		"failed to %s: %s",
		e.Op,
		e.Err)
}

// Unwrap returns the inner error to allow inspection of error chains.
func (e ErrDatabaseOp) Unwrap() error {
	return e.Err
}

// IsNoRows is a utility function for determining if an error wraps the go sql
// package's ErrNoRows, which is returned when a Scan operation has no more
// results to return, and as such is returned by many borp methods.
func IsNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

// IsDuplicate is a utility function for determining if an error wrap MySQL's
// Error 1062: Duplicate entry. This error is returned when inserting a row
// would violate a unique key constraint.
func IsDuplicate(err error) bool {
	var dbErr *mysql.MySQLError
	return errors.As(err, &dbErr) && dbErr.Number == 1062
}

// WrappedMap wraps a *borp.DbMap such that its major functions wrap error
// results in ErrDatabaseOp instances before returning them to the caller.
type WrappedMap struct {
	*borp.DbMap
}

func (m *WrappedMap) Get(holder interface{}, keys ...interface{}) (interface{}, error) {
	return WrappedExecutor{SqlExecutor: m.DbMap}.Get(holder, keys...)
}

func (m *WrappedMap) Insert(list ...interface{}) error {
	return WrappedExecutor{SqlExecutor: m.DbMap}.Insert(list...)
}

func (m *WrappedMap) Update(list ...interface{}) (int64, error) {
	return WrappedExecutor{SqlExecutor: m.DbMap}.Update(list...)
}

func (m *WrappedMap) Delete(list ...interface{}) (int64, error) {
	return WrappedExecutor{SqlExecutor: m.DbMap}.Delete(list...)
}

func (m *WrappedMap) Select(holder interface{}, query string, args ...interface{}) ([]interface{}, error) {
	return WrappedExecutor{SqlExecutor: m.DbMap}.Select(holder, query, args...)
}

func (m *WrappedMap) SelectOne(holder interface{}, query string, args ...interface{}) error {
	return WrappedExecutor{SqlExecutor: m.DbMap}.SelectOne(holder, query, args...)
}

func (m *WrappedMap) Query(query string, args ...interface{}) (*sql.Rows, error) {
	return WrappedExecutor{SqlExecutor: m.DbMap}.Query(query, args...)
}

func (m *WrappedMap) Exec(query string, args ...interface{}) (sql.Result, error) {
	return WrappedExecutor{SqlExecutor: m.DbMap}.Exec(query, args...)
}

func (m *WrappedMap) WithContext(ctx context.Context) borp.SqlExecutor {
	return WrappedExecutor{SqlExecutor: m.DbMap.WithContext(ctx)}
}

func (m *WrappedMap) Begin() (Transaction, error) {
	tx, err := m.DbMap.Begin()
	if err != nil {
		return tx, ErrDatabaseOp{
			Op:  "begin transaction",
			Err: err,
		}
	}
	return WrappedTransaction{
		Transaction: tx,
	}, err
}

// WrappedTransaction wraps a *borp.Transaction such that its major functions
// wrap error results in ErrDatabaseOp instances before returning them to the
// caller.
type WrappedTransaction struct {
	*borp.Transaction
}

func (tx WrappedTransaction) WithContext(ctx context.Context) borp.SqlExecutor {
	return WrappedExecutor{SqlExecutor: tx.Transaction.WithContext(ctx)}
}

func (tx WrappedTransaction) Commit() error {
	return tx.Transaction.Commit()
}

func (tx WrappedTransaction) Rollback() error {
	return tx.Transaction.Rollback()
}

func (tx WrappedTransaction) Get(holder interface{}, keys ...interface{}) (interface{}, error) {
	return (WrappedExecutor{SqlExecutor: tx.Transaction}).Get(holder, keys...)
}

func (tx WrappedTransaction) Insert(list ...interface{}) error {
	return (WrappedExecutor{SqlExecutor: tx.Transaction}).Insert(list...)
}

func (tx WrappedTransaction) Update(list ...interface{}) (int64, error) {
	return (WrappedExecutor{SqlExecutor: tx.Transaction}).Update(list...)
}

func (tx WrappedTransaction) Delete(list ...interface{}) (int64, error) {
	return (WrappedExecutor{SqlExecutor: tx.Transaction}).Delete(list...)
}

func (tx WrappedTransaction) Select(holder interface{}, query string, args ...interface{}) ([]interface{}, error) {
	return (WrappedExecutor{SqlExecutor: tx.Transaction}).Select(holder, query, args...)
}

func (tx WrappedTransaction) SelectOne(holder interface{}, query string, args ...interface{}) error {
	return (WrappedExecutor{SqlExecutor: tx.Transaction}).SelectOne(holder, query, args...)
}

func (tx WrappedTransaction) Query(query string, args ...interface{}) (*sql.Rows, error) {
	return (WrappedExecutor{SqlExecutor: tx.Transaction}).Query(query, args...)
}

func (tx WrappedTransaction) Exec(query string, args ...interface{}) (sql.Result, error) {
	return (WrappedExecutor{SqlExecutor: tx.Transaction}).Exec(query, args...)
}

// WrappedExecutor wraps a borp.SqlExecutor such that its major functions
// wrap error results in ErrDatabaseOp instances before returning them to the
// caller.
type WrappedExecutor struct {
	borp.SqlExecutor
}

func errForOp(operation string, err error, list []interface{}) ErrDatabaseOp {
	table := "unknown"
	if len(list) > 0 {
		table = fmt.Sprintf("%T", list[0])
	}
	return ErrDatabaseOp{
		Op:    operation,
		Table: table,
		Err:   err,
	}
}

func errForQuery(query, operation string, err error, list []interface{}) ErrDatabaseOp {
	// Extract the table from the query
	table := tableFromQuery(query)
	if table == "" && len(list) > 0 {
		// If there's no table from the query but there was a list of holder types,
		// use the type from the first element of the list and indicate we failed to
		// extract a table from the query.
		table = fmt.Sprintf("%T (unknown table)", list[0])
	} else if table == "" {
		// If there's no table from the query and no list of holders then all we can
		// say is that the table is unknown.
		table = "unknown table"
	}

	return ErrDatabaseOp{
		Op:    operation,
		Table: table,
		Err:   err,
	}
}

func (we WrappedExecutor) Get(holder interface{}, keys ...interface{}) (interface{}, error) {
	res, err := we.SqlExecutor.Get(holder, keys...)
	if err != nil {
		return res, errForOp("get", err, []interface{}{holder})
	}
	return res, err
}

func (we WrappedExecutor) Insert(list ...interface{}) error {
	err := we.SqlExecutor.Insert(list...)
	if err != nil {
		return errForOp("insert", err, list)
	}
	return nil
}

func (we WrappedExecutor) Update(list ...interface{}) (int64, error) {
	updatedRows, err := we.SqlExecutor.Update(list...)
	if err != nil {
		return updatedRows, errForOp("update", err, list)
	}
	return updatedRows, err
}

func (we WrappedExecutor) Delete(list ...interface{}) (int64, error) {
	deletedRows, err := we.SqlExecutor.Delete(list...)
	if err != nil {
		return deletedRows, errForOp("delete", err, list)
	}
	return deletedRows, err
}

func (we WrappedExecutor) Select(holder interface{}, query string, args ...interface{}) ([]interface{}, error) {
	result, err := we.SqlExecutor.Select(holder, query, args...)
	if err != nil {
		return result, errForQuery(query, "select", err, []interface{}{holder})
	}
	return result, err
}

func (we WrappedExecutor) SelectOne(holder interface{}, query string, args ...interface{}) error {
	err := we.SqlExecutor.SelectOne(holder, query, args...)
	if err != nil {
		return errForQuery(query, "select one", err, []interface{}{holder})
	}
	return nil
}

func (we WrappedExecutor) Query(query string, args ...interface{}) (*sql.Rows, error) {
	rows, err := we.SqlExecutor.Query(query, args...)
	if err != nil {
		return nil, errForQuery(query, "select", err, nil)
	}
	return rows, nil
}

var (
	// selectTableRegexp matches the table name from an SQL select statement
	selectTableRegexp = regexp.MustCompile(`(?i)^\s*select\s+[a-z\d:\.\(\), \_\*` + "`" + `]+\s+from\s+([a-z\d\_,` + "`" + `]+)`)
	// insertTableRegexp matches the table name from an SQL insert statement
	insertTableRegexp = regexp.MustCompile(`(?i)^\s*insert\s+into\s+([a-z\d \_,` + "`" + `]+)\s+(?:set|\()`)
	// updateTableRegexp matches the table name from an SQL update statement
	updateTableRegexp = regexp.MustCompile(`(?i)^\s*update\s+([a-z\d \_,` + "`" + `]+)\s+set`)
	// deleteTableRegexp matches the table name from an SQL delete statement
	deleteTableRegexp = regexp.MustCompile(`(?i)^\s*delete\s+from\s+([a-z\d \_,` + "`" + `]+)\s+where`)

	// tableRegexps is a list of regexps that tableFromQuery will try to use in
	// succession to find the table name for an SQL query. While tableFromQuery
	// isn't used by the higher level borp Insert/Update/Select/etc functions we
	// include regexps for matching inserts, updates, selects, etc because we want
	// to match the correct table when these types of queries are run through
	// Exec().
	tableRegexps = []*regexp.Regexp{
		selectTableRegexp,
		insertTableRegexp,
		updateTableRegexp,
		deleteTableRegexp,
	}
)

// tableFromQuery uses the tableRegexps on the provided query to return the
// associated table name or an empty string if it can't be determined from the
// query.
func tableFromQuery(query string) string {
	for _, r := range tableRegexps {
		if matches := r.FindStringSubmatch(query); len(matches) >= 2 {
			return matches[1]
		}
	}
	return ""
}

func (we WrappedExecutor) Exec(query string, args ...interface{}) (sql.Result, error) {
	res, err := we.SqlExecutor.Exec(query, args...)
	if err != nil {
		return res, errForQuery(query, "exec", err, args)
	}
	return res, nil
}
