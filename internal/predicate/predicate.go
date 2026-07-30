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

// Package predicate derives a step's precondition from its SQL.
//
// Inference is the ergonomic core: a migration author should almost never have
// to write a predicate by hand, because a hand-written one that is subtly wrong
// is worse than none at all.
package predicate

import (
	"context"
	"fmt"
	"strings"

	"github.com/tochemey/mig/internal/catalog"
	"github.com/tochemey/mig/internal/parse"
	"github.com/tochemey/mig/internal/step"
)

// Check is an inferred precondition together with the words for it.
//
// The description is derived from the same switch as the predicate, so what
// `mig plan` prints and what the executor evaluates cannot drift apart.
type Check struct {
	Satisfied step.Predicate
	Describe  string
}

// Infer derives a check from a step's statements, returning nil when it cannot.
//
// A step is satisfied only when every one of its statements is, so the result
// is the conjunction. One unclassified statement makes the whole step
// uninferable: a partly-known step would report finished work while some of it
// was still outstanding.
func Infer(statements []parse.Statement) *Check {
	if len(statements) == 0 {
		return nil
	}

	var (
		checks    = make([]step.Predicate, 0, len(statements))
		described = make([]string, 0, len(statements))
	)

	for _, stmt := range statements {
		check := inferOne(stmt)
		if check == nil {
			return nil
		}

		checks = append(checks, check.Satisfied)
		described = append(described, check.Describe)
	}

	return &Check{Satisfied: all(checks), Describe: strings.Join(described, ", and ")}
}

// inferOne derives the check for a single statement.
//
// The mapping is the whole of the design's inference table. A statement absent
// from it carries no check, and a non-transactional step holding one is refused
// at plan time rather than becoming an unreconcilable step at run time.
func inferOne(stmt parse.Statement) *Check {
	target := stmt.Target

	switch stmt.Kind {
	case parse.KindCreateIndex:
		return &Check{
			Satisfied: indexUsable(target),
			Describe:  fmt.Sprintf("index %s exists and is valid and ready", target.Qualified()),
		}

	case parse.KindDropIndex:
		return &Check{
			Satisfied: indexAbsent(target),
			Describe:  fmt.Sprintf("index %s is absent", target.Qualified()),
		}

	case parse.KindAddColumn:
		return &Check{
			Satisfied: columnExists(target),
			Describe:  fmt.Sprintf("column %s exists", target.Qualified()),
		}

	case parse.KindDropColumn:
		return &Check{
			Satisfied: columnAbsent(target),
			Describe: fmt.Sprintf("table %s exists and column %s does not",
				target.Name, target.Qualified()),
		}

	case parse.KindAddConstraint:
		return &Check{
			Satisfied: constraintExists(target),
			Describe:  fmt.Sprintf("constraint %s exists", target.Qualified()),
		}

	case parse.KindValidateConstraint:
		return &Check{
			Satisfied: constraintValidated(target),
			Describe:  fmt.Sprintf("constraint %s exists and is validated", target.Qualified()),
		}

	case parse.KindCreateTable:
		return &Check{
			Satisfied: relationExists(target),
			Describe:  fmt.Sprintf("relation %s exists", target.Qualified()),
		}

	case parse.KindAddEnumValue:
		return &Check{
			Satisfied: enumHasLabel(target),
			Describe:  fmt.Sprintf("enum %s has label %q", target.Name, target.Member),
		}

	case parse.KindOther:
		return nil

	default:
		return nil
	}
}

// indexUsable is satisfied when the index exists and the build finished.
//
// Existence alone is not enough. An interrupted concurrent build leaves an
// index that exists and is not valid, and treating that as done would ship a
// migration that the planner ignores.
func indexUsable(target parse.Target) step.Predicate {
	return func(ctx context.Context, q catalog.Querier) (bool, error) {
		index, err := catalog.LookupIndex(ctx, q, target.Schema, target.Name)
		if err != nil {
			return false, err
		}

		return index.Usable(), nil
	}
}

// indexAbsent is satisfied when the index is gone.
func indexAbsent(target parse.Target) step.Predicate {
	return func(ctx context.Context, q catalog.Querier) (bool, error) {
		index, err := catalog.LookupIndex(ctx, q, target.Schema, target.Name)
		if err != nil {
			return false, err
		}

		return !index.Exists, nil
	}
}

// columnExists is satisfied once the column is on the table.
func columnExists(target parse.Target) step.Predicate {
	return func(ctx context.Context, q catalog.Querier) (bool, error) {
		return catalog.LookupColumn(ctx, q, target.Schema, target.Name, target.Member)
	}
}

// columnAbsent is satisfied when the table is there and the column is not.
//
// The table is checked as well because a column of a table that does not exist
// is absent for the wrong reason. Reporting the drop as done would hide a
// migration series in which the table was never created, and hiding it is the
// opposite of what asking the catalog is for.
func columnAbsent(target parse.Target) step.Predicate {
	return func(ctx context.Context, q catalog.Querier) (bool, error) {
		table, err := catalog.LookupRelation(ctx, q, target.Schema, target.Name)
		if err != nil || !table {
			return false, err
		}

		exists, err := catalog.LookupColumn(ctx, q, target.Schema, target.Name, target.Member)
		if err != nil {
			return false, err
		}

		return !exists, nil
	}
}

// constraintExists is satisfied once the constraint is on the table, whether or
// not it has been validated. Adding NOT VALID and validating are separate
// steps, and each has to be able to tell that its own work is done.
func constraintExists(target parse.Target) step.Predicate {
	return func(ctx context.Context, q catalog.Querier) (bool, error) {
		constraint, err := catalog.LookupConstraint(ctx, q, target.Schema, target.Name, target.Member)
		if err != nil {
			return false, err
		}

		return constraint.Exists, nil
	}
}

// constraintValidated is satisfied once the constraint has been checked against
// every existing row.
func constraintValidated(target parse.Target) step.Predicate {
	return func(ctx context.Context, q catalog.Querier) (bool, error) {
		constraint, err := catalog.LookupConstraint(ctx, q, target.Schema, target.Name, target.Member)
		if err != nil {
			return false, err
		}

		return constraint.Exists && constraint.Validated, nil
	}
}

// relationExists is satisfied once the relation is there.
func relationExists(target parse.Target) step.Predicate {
	return func(ctx context.Context, q catalog.Querier) (bool, error) {
		return catalog.LookupRelation(ctx, q, target.Schema, target.Name)
	}
}

// enumHasLabel is satisfied once the enum carries the label.
func enumHasLabel(target parse.Target) step.Predicate {
	return func(ctx context.Context, q catalog.Querier) (bool, error) {
		return catalog.LookupEnumLabel(ctx, q, target.Schema, target.Name, target.Member)
	}
}

// all is satisfied when every check is.
func all(checks []step.Predicate) step.Predicate {
	return func(ctx context.Context, q catalog.Querier) (bool, error) {
		for _, check := range checks {
			ok, err := check(ctx, q)
			if err != nil || !ok {
				return false, err
			}
		}

		return true, nil
	}
}

// SQL builds a check from a user-supplied expression, the escape hatch for
// steps that cannot be inferred. The expression must return a single boolean.
func SQL(expr string) *Check {
	satisfied := func(ctx context.Context, q catalog.Querier) (bool, error) {
		var done bool

		//nolint:gosec // G201: the expression is the migration author's own SQL.
		if err := q.QueryRowContext(ctx, expr).Scan(&done); err != nil {
			return false, fmt.Errorf("evaluate satisfied predicate %q: %w", expr, err)
		}

		return done, nil
	}

	return &Check{Satisfied: satisfied, Describe: "author-supplied: " + expr}
}
