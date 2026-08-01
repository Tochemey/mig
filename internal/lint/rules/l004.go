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

package rules

import (
	"fmt"
	"slices"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/tochemey/mig/internal/lint/fix"
	"github.com/tochemey/mig/internal/lint/history"
)

// l004 flags ALTER COLUMN TYPE. When the plan's own DDL shows the prior type
// and the change is one Postgres alters in place, there is no rewrite to warn
// about. Otherwise the model cannot rule the rewrite out offline, so the rule
// warns and the connected mode refines it later.
type l004 struct{}

func (l004) ID() string { return L004 }

func (l004) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	alter, cmds := alterCommands(stmt)
	if alter == nil {
		return nil
	}

	var (
		change   *pgquery.AlterTableCmd
		rewrites bool
	)

	schema, name := tableOf(alter)

	for _, cmd := range cmds {
		if cmd.GetSubtype() != pgquery.AlterTableType_AT_AlterColumnType {
			continue
		}

		change = cmd
		rewrites = rewrites || !metadataOnly(ctx, name, cmd)
	}

	if change == nil || !rewrites {
		return nil
	}

	if ctx.createdHere(schema, name) {
		return nil
	}

	found := sized(ctx, schema, name, fmt.Sprintf(
		"changing a column type rewrites %s under ACCESS EXCLUSIVE; "+
			"add a new column, backfill, swap reads, then drop the old one",
		qualified(schema, name)))

	// The replacement is a scaffold: the write gap between backfill and swap
	// is not a pure SQL problem, so the plan arrives commented out.
	if len(cmds) == 1 {
		found = withFix(found, fix.TypeChangeScaffold(alter.GetRelation(), change, pagingKey(ctx, schema, name)))
	}

	return found
}

// metadataOnly reports whether the type change is one Postgres performs
// without touching the rows, which needs the prior type: the plan's own DDL
// is asked for it, and an unknown prior keeps the conservative answer.
//
// The in-place changes recognised are the documented ones: widening varchar,
// unbinding it, varchar or char-free text moves inside the varchar family,
// and widening numeric's precision at the same scale. Everything else,
// including every change of type family, answers false.
func metadataOnly(ctx Context, table string, cmd *pgquery.AlterTableCmd) bool {
	prior, known := ctx.History.ColumnTypeBefore(table, cmd.GetName(),
		ctx.Migration.File, ctx.StepIndex, ctx.StmtIndex)
	if !known {
		return false
	}

	// The zero spec a malformed tree would produce matches no known prior,
	// so it keeps the warning like any other unrecognised change.
	next, _ := history.SpecOf(cmd.GetDef().GetColumnDef().GetTypeName())

	if prior.Name == next.Name && slices.Equal(prior.Mods, next.Mods) {
		return true
	}

	switch {
	case prior.Name == "varchar" && next.Name == "varchar":
		// Widening the limit, or removing it, changes no stored byte.
		return len(next.Mods) == 0 || (len(prior.Mods) == 1 && next.Mods[0] >= prior.Mods[0])

	case prior.Name == "varchar" && next.Name == "text":
		return true

	case prior.Name == "numeric" && next.Name == "numeric":
		// More precision at the same scale, or unconstrained, fits every
		// existing value where it already is.
		if len(next.Mods) == 0 {
			return true
		}

		return len(prior.Mods) == 2 && len(next.Mods) == 2 &&
			next.Mods[0] >= prior.Mods[0] && next.Mods[1] == prior.Mods[1]

	default:
		return false
	}
}
