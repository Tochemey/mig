// MIT License
//
// Copyright (c) 2026 Arsene Tochemey Gandote
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
//

package fix

import (
	"fmt"
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"
)

// backfillBatch is the batch size generated backfills start at.
const backfillBatch = 5000

// assumedKey is the cursor column a generated backfill falls back to. Offline,
// nothing says which column is the primary key; the step's comment tells the
// reviewer to correct it, and a wrong assumption fails loudly at run time
// rather than quietly doing the wrong thing. Connected, the catalog names the
// real one and the comment says so instead.
const assumedKey = "id"

// pagingKey picks the column a generated backfill pages by, and explains the
// choice in the step's own comment.
func pagingKey(key string) (column, note string) {
	if key == "" {
		return assumedKey, "key=" + assumedKey + " is assumed, point it at the primary key if that is not right"
	}

	return key, "key=" + key + " is the table's primary key"
}

// AddColumnWithDefault replaces ADD COLUMN with a rewriting default by the
// expand route: the column arrives empty, the default applies to future rows
// only, old rows are backfilled in batches, and NOT NULL, when the original
// asked for it, arrives through a validated check.
func AddColumnWithDefault(rel *pgquery.RangeVar, column *pgquery.ColumnDef,
	def *pgquery.Node, notNull bool, key string) *Fix {
	name := rel.GetRelname() + "_" + column.GetColname()
	col := column.GetColname()
	cursor, note := pagingKey(key)

	bare := dup(column)
	bare.Constraints = nil
	bare.IsNotNull = false

	steps := []Step{
		{
			Name:    "add_" + name + "_nullable",
			Comment: "the column arrives nullable and empty: catalog only, no rewrite",
			Stmts: []*pgquery.Node{alterTable(rel,
				alterCmd(pgquery.AlterTableType_AT_AddColumn, "", columnDefNode(bare)))},
		},
		{
			Name:    "set_" + name + "_default",
			Comment: "future rows get the default; existing rows are left to the backfill",
			Stmts: []*pgquery.Node{alterTable(rel,
				alterCmd(pgquery.AlterTableType_AT_ColumnDefault, col, dup(def)))},
		},
		{
			Name:    "backfill_" + name,
			Comment: "batched so each transaction stays short; " + note,
			Backfill: &Backfill{
				Table:     qualifiedTable(rel),
				Key:       cursor,
				Batch:     backfillBatch,
				Satisfied: noNullsRemain(rel, col),
			},
			Stmts: []*pgquery.Node{backfillUpdate(rel, col, dup(def), cursor)},
		},
	}

	if notNull {
		steps = append(steps, notNullSteps(rel, col)...)
	}

	return &Fix{Steps: steps}
}

// NotNullPattern replaces SET NOT NULL with the validated-check route: the
// check arrives without a scan, is validated without blocking writes, proves
// the column to SET NOT NULL, and leaves.
func NotNullPattern(rel *pgquery.RangeVar, column string) *Fix {
	return &Fix{Steps: notNullSteps(rel, column)}
}

// notNullSteps is the shared tail of [AddColumnWithDefault] and
// [NotNullPattern].
//
// The first three steps carry explicit predicates naming the pattern's goal,
// because the last step undoes what inference would look for: once the
// scaffolding constraint is dropped, "the constraint is validated" is false
// on a fully converged database, and a fresh runner would redo finished
// work. "The column is NOT NULL" stays true.
func notNullSteps(rel *pgquery.RangeVar, column string) []Step {
	prefix := rel.GetRelname() + "_" + column
	name := checkName(rel.GetRelname(), column)

	columnNotNull := fmt.Sprintf(
		"coalesce((SELECT attnotnull FROM pg_attribute WHERE attrelid = %s AND attname = %s), false)",
		regclass(rel), literal(column))

	constraintExists := fmt.Sprintf(
		"EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = %s AND conname = %s)",
		regclass(rel), literal(name))

	constraintValidated := fmt.Sprintf(
		"EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = %s AND conname = %s AND convalidated)",
		regclass(rel), literal(name))

	return []Step{
		{
			Name:      prefix + "_not_null_check",
			Comment:   "NOT VALID: the check exists without scanning anything",
			Satisfied: fmt.Sprintf("SELECT %s OR %s", constraintExists, columnNotNull),
			Stmts: []*pgquery.Node{alterTable(rel,
				alterCmd(pgquery.AlterTableType_AT_AddConstraint, "",
					constraint(checkNotNullConstraint(name, column))))},
		},
		{
			Name:      "validate_" + prefix + "_not_null",
			Comment:   "the scan happens here, without blocking reads or writes",
			NoTx:      true,
			Satisfied: fmt.Sprintf("SELECT %s OR %s", constraintValidated, columnNotNull),
			Stmts: []*pgquery.Node{alterTable(rel,
				alterCmd(pgquery.AlterTableType_AT_ValidateConstraint, name, nil))},
		},
		{
			Name:      "set_" + prefix + "_not_null",
			Comment:   "catalog only from Postgres 12: the validated check is the proof",
			Satisfied: "SELECT " + columnNotNull,
			Stmts: []*pgquery.Node{alterTable(rel,
				alterCmd(pgquery.AlterTableType_AT_SetNotNull, column, nil))},
		},
		{
			Name:      "drop_" + prefix + "_check",
			Comment:   "the column carries NOT NULL itself now; the scaffolding leaves",
			Satisfied: "SELECT NOT " + constraintExists,
			Stmts: []*pgquery.Node{alterTable(rel,
				alterCmd(pgquery.AlterTableType_AT_DropConstraint, name, nil))},
		},
	}
}

// literal quotes a name for use inside predicate SQL as a string value.
func literal(name string) string {
	return "'" + strings.ReplaceAll(name, "'", "''") + "'"
}

// regclass resolves the relation inside predicate SQL.
func regclass(rel *pgquery.RangeVar) string {
	return "to_regclass(" + literal(qualifiedTable(rel)) + ")"
}

// ForeignKeyTwoStep replaces ADD FOREIGN KEY with the NOT VALID form plus a
// separate validation, so the long scan holds SHARE UPDATE EXCLUSIVE instead
// of blocking writes to both tables.
func ForeignKeyTwoStep(rel *pgquery.RangeVar, con *pgquery.Constraint) *Fix {
	return constraintTwoStep(rel, con,
		"NOT VALID: the key holds for new writes at once, no scan yet",
		"the scan happens here, without blocking writes to either table")
}

// CheckTwoStep replaces ADD CHECK with the NOT VALID form plus a separate
// validation, so the scan holds SHARE UPDATE EXCLUSIVE instead of ACCESS
// EXCLUSIVE.
func CheckTwoStep(rel *pgquery.RangeVar, con *pgquery.Constraint) *Fix {
	return constraintTwoStep(rel, con,
		"NOT VALID: the check holds for new rows at once, no scan yet",
		"the scan happens here, blocking neither reads nor writes")
}

// constraintTwoStep defers a constraint's validation out of the statement
// that adds it. The comments differ per constraint kind because what the
// deferred scan stops blocking does.
func constraintTwoStep(rel *pgquery.RangeVar, con *pgquery.Constraint, add, validate string) *Fix {
	name := con.GetConname()

	deferred := dup(con)
	deferred.SkipValidation = true
	deferred.InitiallyValid = false

	return &Fix{Steps: []Step{
		{
			Name:    "add_" + name + "_not_valid",
			Comment: add,
			Stmts: []*pgquery.Node{alterTable(rel,
				alterCmd(pgquery.AlterTableType_AT_AddConstraint, "", constraint(deferred)))},
		},
		{
			Name:    "validate_" + name,
			Comment: validate,
			NoTx:    true,
			Stmts: []*pgquery.Node{alterTable(rel,
				alterCmd(pgquery.AlterTableType_AT_ValidateConstraint, name, nil))},
		},
	}}
}

// PrimaryKeyViaIndex replaces ADD PRIMARY KEY with a concurrent unique index
// build and an adoption, so the index build happens off the ACCESS EXCLUSIVE
// path.
func PrimaryKeyViaIndex(rel *pgquery.RangeVar, con *pgquery.Constraint) *Fix {
	name := con.GetConname()
	if name == "" {
		// The server's own default name for a primary key.
		name = rel.GetRelname() + "_pkey"
	}

	columns := make([]string, 0, len(con.GetKeys()))

	for _, key := range con.GetKeys() {
		columns = append(columns, key.GetString_().GetSval())
	}

	return &Fix{Steps: []Step{
		{
			Name:    "index_" + name,
			Comment: "the expensive build runs concurrently, blocking nothing",
			NoTx:    true,
			Stmts:   []*pgquery.Node{uniqueIndexConcurrently(name, rel, columns)},
		},
		{
			Name: "adopt_" + name,
			Comment: "adoption is catalog work, but verifying NOT NULL still scans; on a " +
				"large table set each key column NOT NULL via a validated check first",
			Stmts: []*pgquery.Node{alterTable(rel,
				alterCmd(pgquery.AlterTableType_AT_AddConstraint, "",
					constraint(&pgquery.Constraint{
						Contype:   pgquery.ConstrType_CONSTR_PRIMARY,
						Conname:   name,
						Indexname: name,
					})))},
		},
	}}
}

// TypeChangeScaffold plans the expand-contract for a column type change. It
// is a scaffold, not a migration: writes landing between the backfill and
// the swap are lost unless the application dual-writes or traffic pauses,
// and that decision cannot be made in SQL.
func TypeChangeScaffold(rel *pgquery.RangeVar, cmd *pgquery.AlterTableCmd, key string) *Fix {
	column := cmd.GetName()
	cursor, note := pagingKey(key)
	newColumn := column + "__new"
	prefix := rel.GetRelname() + "_"
	target := cmd.GetDef().GetColumnDef()

	// ALTER COLUMN TYPE ... USING carries its expression in the column
	// definition; without one, the cast of the old column is the value.
	value := target.GetRawDefault()
	if value == nil {
		value = typeCast(columnRef(column), target.GetTypeName())
	}

	fresh := &pgquery.ColumnDef{Colname: newColumn, TypeName: dup(target.GetTypeName())}

	return &Fix{
		Scaffold: true,
		TODO: "writes landing between the backfill and the swap are lost unless the " +
			"application dual-writes both columns or traffic pauses; decide that, " +
			"then run these steps",
		Steps: []Step{
			{
				Name:    "add_" + prefix + newColumn,
				Comment: "the new column arrives empty beside the old one",
				Stmts: []*pgquery.Node{alterTable(rel,
					alterCmd(pgquery.AlterTableType_AT_AddColumn, "", columnDefNode(fresh)))},
			},
			{
				Name: "backfill_" + prefix + newColumn,
				Comment: fmt.Sprintf(
					"rows where %s is NULL stay NULL; adjust the predicate if the "+
						"column is nullable. %s", column, note),
				Backfill: &Backfill{
					Table:     qualifiedTable(rel),
					Key:       cursor,
					Batch:     backfillBatch,
					Satisfied: noNullsRemain(rel, newColumn),
				},
				Stmts: []*pgquery.Node{backfillUpdate(rel, newColumn, value, cursor)},
			},
			{
				Name:    "swap_" + prefix + column,
				Comment: "one transaction: the old column leaves, the new one takes its name",
				Stmts: []*pgquery.Node{
					alterTable(rel, alterCmd(pgquery.AlterTableType_AT_DropColumn, column, nil)),
					renameColumn(rel, newColumn, column),
				},
			},
		},
	}
}

// checkName names the scaffolding constraint, inside the server's identifier
// limit.
func checkName(table, column string) string {
	name := table + "_" + column + "_nn"
	if len(name) > 63 {
		name = name[:63]
	}

	return name
}

// qualifiedTable renders the relation for a backfill annotation.
func qualifiedTable(rel *pgquery.RangeVar) string {
	if rel.GetSchemaname() == "" {
		return rel.GetRelname()
	}

	return rel.GetSchemaname() + "." + rel.GetRelname()
}
