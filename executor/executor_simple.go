// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"fmt"
	"math/rand"
	"strings"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/context"
	"github.com/pingcap/tidb/evaluator"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/plan/statistics"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/terror"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/sqlexec"
	"github.com/pingcap/tidb/util/types"
)

// SimpleExec represents simple statement executor.
// For statements do simple execution.
// includes `UseStmt`, 'SetStmt`, `DoStmt`,
// `BeginStmt`, `CommitStmt`, `RollbackStmt`.
// TODO: list all simple statements.
type SimpleExec struct {
	Statement ast.StmtNode
	ctx       context.Context
	done      bool
}

// Schema implements the Executor Schema interface.
func (e *SimpleExec) Schema() expression.Schema {
	return nil
}

// Next implements Execution Next interface.
func (e *SimpleExec) Next() (*Row, error) {
	if e.done {
		return nil, nil
	}
	var err error
	switch x := e.Statement.(type) {
	case *ast.UseStmt:
		err = e.executeUse(x)
	case *ast.FlushTableStmt:
		err = e.executeFlushTable(x)
	case *ast.DoStmt:
		err = e.executeDo(x)
	case *ast.BeginStmt:
		err = e.executeBegin(x)
	case *ast.CommitStmt:
		err = e.executeCommit(x)
	case *ast.RollbackStmt:
		err = e.executeRollback(x)
	case *ast.CreateUserStmt:
		err = e.executeCreateUser(x)
	case *ast.DropUserStmt:
		err = e.executeDropUser(x)
	case *ast.SetPwdStmt:
		err = e.executeSetPwd(x)
	case *ast.AnalyzeTableStmt:
		err = e.executeAnalyzeTable(x)
	case *ast.BinlogStmt:
		// We just ignore it.
		return nil, nil
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	e.done = true
	return nil, nil
}

// Close implements the Executor Close interface.
func (e *SimpleExec) Close() error {
	return nil
}

func (e *SimpleExec) executeUse(s *ast.UseStmt) error {
	dbname := model.NewCIStr(s.DBName)
	dbinfo, exists := sessionctx.GetDomain(e.ctx).InfoSchema().SchemaByName(dbname)
	if !exists {
		return infoschema.ErrDatabaseNotExists.GenByArgs(dbname)
	}
	e.ctx.GetSessionVars().CurrentDB = dbname.O
	// character_set_database is the character set used by the default database.
	// The server sets this variable whenever the default database changes.
	// See http://dev.mysql.com/doc/refman/5.7/en/server-system-variables.html#sysvar_character_set_database
	sessionVars := e.ctx.GetSessionVars()
	sessionVars.Systems[variable.CharsetDatabase] = dbinfo.Charset
	sessionVars.Systems[variable.CollationDatabase] = dbinfo.Collate
	return nil
}

func (e *SimpleExec) executeDo(s *ast.DoStmt) error {
	for _, expr := range s.Exprs {
		_, err := evaluator.Eval(e.ctx, expr)
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func (e *SimpleExec) executeBegin(s *ast.BeginStmt) error {
	_, err := e.ctx.GetTxn(true)
	if err != nil {
		return errors.Trace(err)
	}
	// With START TRANSACTION, autocommit remains disabled until you end
	// the transaction with COMMIT or ROLLBACK. The autocommit mode then
	// reverts to its previous state.
	e.ctx.GetSessionVars().SetStatusFlag(mysql.ServerStatusInTrans, true)
	return nil
}

func (e *SimpleExec) executeCommit(s *ast.CommitStmt) error {
	err := e.ctx.CommitTxn()
	e.ctx.GetSessionVars().SetStatusFlag(mysql.ServerStatusInTrans, false)
	return errors.Trace(err)
}

func (e *SimpleExec) executeRollback(s *ast.RollbackStmt) error {
	sessVars := e.ctx.GetSessionVars()
	log.Infof("[%d] execute rollback statement", sessVars.ConnectionID)
	err := e.ctx.RollbackTxn()
	sessVars.SetStatusFlag(mysql.ServerStatusInTrans, false)
	return errors.Trace(err)
}

func (e *SimpleExec) executeCreateUser(s *ast.CreateUserStmt) error {
	users := make([]string, 0, len(s.Specs))
	for _, spec := range s.Specs {
		userName, host := parseUser(spec.User)
		exists, err1 := userExists(e.ctx, userName, host)
		if err1 != nil {
			return errors.Trace(err1)
		}
		if exists {
			if !s.IfNotExists {
				return errors.New("Duplicate user")
			}
			continue
		}
		pwd := ""
		if spec.AuthOpt != nil {
			if spec.AuthOpt.ByAuthString {
				pwd = util.EncodePassword(spec.AuthOpt.AuthString)
			} else {
				pwd = util.EncodePassword(spec.AuthOpt.HashString)
			}
		}
		user := fmt.Sprintf(`("%s", "%s", "%s")`, host, userName, pwd)
		users = append(users, user)
	}
	if len(users) == 0 {
		return nil
	}
	sql := fmt.Sprintf(`INSERT INTO %s.%s (Host, User, Password) VALUES %s;`, mysql.SystemDB, mysql.UserTable, strings.Join(users, ", "))
	_, err := e.ctx.(sqlexec.RestrictedSQLExecutor).ExecRestrictedSQL(e.ctx, sql)
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

func (e *SimpleExec) executeDropUser(s *ast.DropUserStmt) error {
	failedUsers := make([]string, 0, len(s.UserList))
	for _, user := range s.UserList {
		userName, host := parseUser(user)
		exists, err := userExists(e.ctx, userName, host)
		if err != nil {
			return errors.Trace(err)
		}
		if !exists {
			if !s.IfExists {
				failedUsers = append(failedUsers, user)
			}
			continue
		}
		sql := fmt.Sprintf(`DELETE FROM %s.%s WHERE Host = "%s" and User = "%s";`, mysql.SystemDB, mysql.UserTable, host, userName)
		_, err = e.ctx.(sqlexec.RestrictedSQLExecutor).ExecRestrictedSQL(e.ctx, sql)
		if err != nil {
			failedUsers = append(failedUsers, user)
		}
	}
	err := e.ctx.CommitTxn()
	if err != nil {
		return errors.Trace(err)
	}
	if len(failedUsers) > 0 {
		errMsg := "Operation DROP USER failed for " + strings.Join(failedUsers, ",")
		return terror.ClassExecutor.New(CodeCannotUser, errMsg)
	}
	return nil
}

// parse user string into username and host
// root@localhost -> root, localhost
func parseUser(user string) (string, string) {
	strs := strings.Split(user, "@")
	return strs[0], strs[1]
}

func userExists(ctx context.Context, name string, host string) (bool, error) {
	sql := fmt.Sprintf(`SELECT * FROM %s.%s WHERE User="%s" AND Host="%s";`, mysql.SystemDB, mysql.UserTable, name, host)
	rs, err := ctx.(sqlexec.RestrictedSQLExecutor).ExecRestrictedSQL(ctx, sql)
	if err != nil {
		return false, errors.Trace(err)
	}
	defer rs.Close()
	row, err := rs.Next()
	if err != nil {
		return false, errors.Trace(err)
	}
	return row != nil, nil
}

func (e *SimpleExec) executeSetPwd(s *ast.SetPwdStmt) error {
	// TODO: If len(s.User) == 0, use CURRENT_USER()
	userName, host := parseUser(s.User)
	// Update mysql.user
	sql := fmt.Sprintf(`UPDATE %s.%s SET password="%s" WHERE User="%s" AND Host="%s";`, mysql.SystemDB, mysql.UserTable, util.EncodePassword(s.Password), userName, host)
	_, err := e.ctx.(sqlexec.RestrictedSQLExecutor).ExecRestrictedSQL(e.ctx, sql)
	return errors.Trace(err)
}

func (e *SimpleExec) executeFlushTable(s *ast.FlushTableStmt) error {
	// TODO: A dummy implement
	return nil
}

func (e *SimpleExec) executeAnalyzeTable(s *ast.AnalyzeTableStmt) error {
	for _, table := range s.TableNames {
		err := e.createStatisticsForTable(table)
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

const (
	maxSampleCount     = 10000
	defaultBucketCount = 256
)

func (e *SimpleExec) createStatisticsForTable(tn *ast.TableName) error {
	var tableName string
	if tn.Schema.L == "" {
		tableName = tn.Name.L
	} else {
		tableName = tn.Schema.L + "." + tn.Name.L
	}
	sql := "select * from " + tableName
	result, err := e.ctx.(sqlexec.RestrictedSQLExecutor).ExecRestrictedSQL(e.ctx, sql)
	if err != nil {
		return errors.Trace(err)
	}
	count, samples, err := e.collectSamples(result)
	result.Close()
	if err != nil {
		return errors.Trace(err)
	}
	err = e.buildStatisticsAndSaveToKV(tn, count, samples)
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

// collectSamples collects sample from the result set, using Reservoir Sampling algorithm.
// See https://en.wikipedia.org/wiki/Reservoir_sampling
func (e *SimpleExec) collectSamples(result ast.RecordSet) (count int64, samples []*ast.Row, err error) {
	for {
		var row *ast.Row
		row, err = result.Next()
		if err != nil {
			return count, samples, errors.Trace(err)
		}
		if row == nil {
			break
		}
		if len(samples) < maxSampleCount {
			samples = append(samples, row)
		} else {
			shouldAdd := rand.Int63n(count) < maxSampleCount
			if shouldAdd {
				idx := rand.Intn(maxSampleCount)
				samples[idx] = row
			}
		}
		count++
	}
	return count, samples, nil
}

func (e *SimpleExec) buildStatisticsAndSaveToKV(tn *ast.TableName, count int64, sampleRows []*ast.Row) error {
	txn, err := e.ctx.GetTxn(false)
	if err != nil {
		return errors.Trace(err)
	}
	columnSamples := rowsToColumnSamples(sampleRows)
	t, err := statistics.NewTable(tn.TableInfo, int64(txn.StartTS()), count, defaultBucketCount, columnSamples)
	if err != nil {
		return errors.Trace(err)
	}
	tpb, err := t.ToPB()
	if err != nil {
		return errors.Trace(err)
	}
	m := meta.NewMeta(txn)
	err = m.SetTableStats(tn.TableInfo.ID, tpb)
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

func rowsToColumnSamples(rows []*ast.Row) [][]types.Datum {
	if len(rows) == 0 {
		return nil
	}
	columnSamples := make([][]types.Datum, len(rows[0].Data))
	for i := range columnSamples {
		columnSamples[i] = make([]types.Datum, len(rows))
	}
	for j, row := range rows {
		for i, val := range row.Data {
			columnSamples[i][j] = val
		}
	}
	return columnSamples
}
