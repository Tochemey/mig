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

	"github.com/tochemey/mig/internal/catalog"
	"github.com/tochemey/mig/internal/parse"
	"github.com/tochemey/mig/internal/step"
)

// Infer derives a predicate from a step's statements, returning nil when it
// cannot.
//
// A step is satisfied only when every one of its statements is, so the result
// is the conjunction. One unclassified statement makes the whole step
// uninferable: a partly-known step would report finished work while some of it
// was still outstanding.
func Infer(statements []parse.Statement) step.Predicate {
	if len(statements) == 0 {
		return nil
	}

	checks := make([]step.Predicate, 0, len(statements))

	for _, stmt := range statements {
		check := inferOne(stmt)
		if check == nil {
			return nil
		}

		checks = append(checks, check)
	}

	return all(checks)
}

// inferOne derives the predicate for a single statement.
func inferOne(stmt parse.Statement) step.Predicate {
	switch stmt.Kind {
	case parse.KindCreateIndex:
		return indexUsable(stmt.Index)

	case parse.KindDropIndex:
		return indexAbsent(stmt.Index)

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
func indexUsable(target parse.Index) step.Predicate {
	return func(ctx context.Context, q catalog.Querier) (bool, error) {
		index, err := catalog.LookupIndex(ctx, q, target.Schema, target.Name)
		if err != nil {
			return false, err
		}

		return index.Usable(), nil
	}
}

// indexAbsent is satisfied when the index is gone.
func indexAbsent(target parse.Index) step.Predicate {
	return func(ctx context.Context, q catalog.Querier) (bool, error) {
		index, err := catalog.LookupIndex(ctx, q, target.Schema, target.Name)
		if err != nil {
			return false, err
		}

		return !index.Exists, nil
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

// SQL builds a predicate from a user-supplied expression, the escape hatch for
// steps that cannot be inferred. The expression must return a single boolean.
func SQL(expr string) step.Predicate {
	return func(ctx context.Context, q catalog.Querier) (bool, error) {
		var satisfied bool

		//nolint:gosec // G201: the expression is the migration author's own SQL.
		if err := q.QueryRowContext(ctx, expr).Scan(&satisfied); err != nil {
			return false, fmt.Errorf("evaluate satisfied predicate %q: %w", expr, err)
		}

		return satisfied, nil
	}
}
