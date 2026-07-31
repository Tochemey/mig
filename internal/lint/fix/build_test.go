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
	"strings"
	"testing"

	pgquery "github.com/pganalyze/pg_query_go/v6"
)

// alterOf parses an ALTER TABLE statement down to its parts.
func alterOf(t *testing.T, sql string) (*pgquery.RangeVar, *pgquery.AlterTableCmd) {
	t.Helper()

	alter := stmt(t, sql).GetAlterTableStmt()

	return alter.GetRelation(), alter.GetCmds()[0].GetAlterTableCmd()
}

// render fails the test on a rendering error.
func render(t *testing.T, f *Fix) string {
	t.Helper()

	out, err := f.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	return out
}

// contains asserts every want line appears, in order.
func contains(t *testing.T, got string, wants []string) {
	t.Helper()

	rest := got

	for _, want := range wants {
		index := strings.Index(rest, want)
		if index < 0 {
			t.Fatalf("output lacks %q in order:\n%s", want, got)
		}

		rest = rest[index+len(want):]
	}
}

func TestAddColumnWithDefault(t *testing.T) {
	rel, cmd := alterOf(t,
		"ALTER TABLE users ADD COLUMN email text NOT NULL DEFAULT gen_email()")

	column := cmd.GetDef().GetColumnDef()

	var def *pgquery.Node

	for _, node := range column.GetConstraints() {
		if node.GetConstraint().GetContype() == pgquery.ConstrType_CONSTR_DEFAULT {
			def = node.GetConstraint().GetRawExpr()
		}
	}

	got := render(t, AddColumnWithDefault(rel, column, def, true))

	contains(t, got, []string{
		"-- +mig step: add_users_email_nullable\n",
		"ALTER TABLE users ADD COLUMN email text;\n",
		"-- +mig step: set_users_email_default\n",
		"ALTER TABLE users ALTER COLUMN email SET DEFAULT gen_email();\n",
		"-- +mig step: backfill_users_email\n",
		"-- +mig backfill: table=users key=id batch=5000\n",
		"-- +mig satisfied: sql(SELECT NOT EXISTS (SELECT 1 FROM users WHERE email IS NULL))\n",
		"UPDATE users SET email = gen_email() " +
			"WHERE id > :cursor_lo AND id <= :cursor_hi AND email IS NULL;\n",
		"ALTER TABLE users ADD CONSTRAINT users_email_nn CHECK (email IS NOT NULL) NOT VALID;\n",
		"-- +mig notx\n",
		"ALTER TABLE users VALIDATE CONSTRAINT users_email_nn;\n",
		"ALTER TABLE users ALTER COLUMN email SET NOT NULL;\n",
		"ALTER TABLE users DROP CONSTRAINT users_email_nn;\n",
	})

	// The original column carried NOT NULL; stripping it must not have
	// mutated the caller's tree.
	if len(column.GetConstraints()) != 2 {
		t.Error("the builder mutated the caller's column definition")
	}
}

func TestAddColumnWithoutNotNullStopsAtTheBackfill(t *testing.T) {
	rel, cmd := alterOf(t, "ALTER TABLE app.users ADD COLUMN score int DEFAULT 0")

	column := cmd.GetDef().GetColumnDef()
	def := column.GetConstraints()[0].GetConstraint().GetRawExpr()

	got := render(t, AddColumnWithDefault(rel, column, def, false))

	if strings.Contains(got, "NOT NULL") {
		t.Errorf("a nullable column grew a not-null tail:\n%s", got)
	}

	// A qualified table stays qualified in the annotation and the SQL.
	contains(t, got, []string{
		"-- +mig backfill: table=app.users key=id batch=5000\n",
		"UPDATE app.users SET score = 0",
	})
}

func TestNotNullPattern(t *testing.T) {
	rel, _ := alterOf(t, "ALTER TABLE users ALTER COLUMN name SET NOT NULL")

	got := render(t, NotNullPattern(rel, "name"))

	contains(t, got, []string{
		"ALTER TABLE users ADD CONSTRAINT users_name_nn CHECK (name IS NOT NULL) NOT VALID;\n",
		"ALTER TABLE users VALIDATE CONSTRAINT users_name_nn;\n",
		"ALTER TABLE users ALTER COLUMN name SET NOT NULL;\n",
		"ALTER TABLE users DROP CONSTRAINT users_name_nn;\n",
	})
}

func TestForeignKeyTwoStep(t *testing.T) {
	rel, cmd := alterOf(t,
		"ALTER TABLE orders ADD CONSTRAINT orders_fk FOREIGN KEY (uid) REFERENCES users (id)")

	con := cmd.GetDef().GetConstraint()
	got := render(t, ForeignKeyTwoStep(rel, con))

	contains(t, got, []string{
		"ALTER TABLE orders ADD CONSTRAINT orders_fk " +
			"FOREIGN KEY (uid) REFERENCES users (id) NOT VALID;\n",
		"-- +mig notx\n",
		"ALTER TABLE orders VALIDATE CONSTRAINT orders_fk;\n",
	})

	if con.GetSkipValidation() {
		t.Error("the builder mutated the caller's constraint")
	}
}

func TestPrimaryKeyViaIndex(t *testing.T) {
	rel, cmd := alterOf(t, "ALTER TABLE users ADD CONSTRAINT users_pk PRIMARY KEY (id, org)")

	got := render(t, PrimaryKeyViaIndex(rel, cmd.GetDef().GetConstraint()))

	contains(t, got, []string{
		"-- +mig notx\n",
		"CREATE UNIQUE INDEX CONCURRENTLY users_pk ON users USING btree (id, org);\n",
		"ALTER TABLE users ADD CONSTRAINT users_pk PRIMARY KEY USING INDEX users_pk;\n",
	})
}

func TestPrimaryKeyViaIndexNamesAnAnonymousKey(t *testing.T) {
	rel, cmd := alterOf(t, "ALTER TABLE users ADD PRIMARY KEY (id)")

	got := render(t, PrimaryKeyViaIndex(rel, cmd.GetDef().GetConstraint()))

	contains(t, got, []string{
		"CREATE UNIQUE INDEX CONCURRENTLY users_pkey ON users USING btree (id);\n",
		"ALTER TABLE users ADD CONSTRAINT users_pkey PRIMARY KEY USING INDEX users_pkey;\n",
	})
}

func TestTypeChangeScaffold(t *testing.T) {
	rel, cmd := alterOf(t, "ALTER TABLE users ALTER COLUMN id TYPE bigint")

	f := TypeChangeScaffold(rel, cmd)
	if !f.Scaffold {
		t.Fatal("a type change came back executable")
	}

	got := render(t, f)

	contains(t, got, []string{
		"-- TODO: writes landing between the backfill and the swap are lost",
		"-- ALTER TABLE users ADD COLUMN id__new bigint;\n",
		"-- UPDATE users SET id__new = id::bigint",
		"-- ALTER TABLE users DROP id;\n",
		"-- ALTER TABLE users RENAME COLUMN id__new TO id;\n",
	})
}

func TestTypeChangeScaffoldKeepsTheUsingExpression(t *testing.T) {
	rel, cmd := alterOf(t,
		"ALTER TABLE users ALTER COLUMN flags TYPE jsonb USING to_jsonb(flags)")

	got := render(t, TypeChangeScaffold(rel, cmd))

	contains(t, got, []string{
		"-- UPDATE users SET flags__new = to_jsonb(flags)",
	})
}

func TestCheckNameStaysWithinTheIdentifierLimit(t *testing.T) {
	long := strings.Repeat("a", 60)

	if name := checkName(long, long); len(name) != 63 {
		t.Errorf("checkName produced %d bytes", len(name))
	}
}
