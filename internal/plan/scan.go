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

package plan

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/tochemey/mig/internal/parse"
	"github.com/tochemey/mig/internal/step"
)

// prefix marks a line as an annotation rather than SQL or a comment.
const prefix = "-- +mig "

// The annotations.
const (
	annStep          = "step:"
	annNoTx          = "notx"
	annSatisfied     = "satisfied:"
	annNoLockTimeout = "no_lock_timeout"
)

// sqlPredicatePrefix wraps an author-supplied predicate expression.
const sqlPredicatePrefix = "sql("

// ErrUnknownAnnotation reports an annotation this build does not understand.
// Ignoring it would silently drop an instruction the author believed was
// honoured.
var ErrUnknownAnnotation = errors.New("unknown annotation")

// ErrEmptyStep reports a step with a name and no statements.
var ErrEmptyStep = errors.New("step has no statements")

// scan splits a migration's contents into steps.
//
// SQL appearing before any step: annotation belongs to an implicit step, so a
// plain unannotated .sql file is a valid single-step migration and adoption
// costs nothing.
func scan(content string) ([]Step, error) {
	var (
		steps   []Step
		current *Step
		body    []string
	)

	// flush finishes the step being accumulated.
	flush := func() error {
		if current == nil {
			return nil
		}

		if err := finish(current, strings.Join(body, "\n")); err != nil {
			return err
		}

		steps = append(steps, *current)
		current = nil
		body = nil

		return nil
	}

	// begin starts a step, naming it implicitly when the author did not.
	begin := func(name string) error {
		if err := flush(); err != nil {
			return err
		}

		if name == "" {
			name = fmt.Sprintf("step_%d", len(steps)+1)
		}

		current = &Step{Index: len(steps), Name: name, Kind: step.KindDDLTx}

		return nil
	}

	lines := bufio.NewScanner(strings.NewReader(content))
	for lines.Scan() {
		line := lines.Text()

		annotation, ok := annotationOf(line)
		if !ok {
			if strings.TrimSpace(line) == "" && current == nil {
				continue
			}

			if current == nil {
				if err := begin(""); err != nil {
					return nil, err
				}
			}

			body = append(body, line)

			continue
		}

		if name, isStep := strings.CutPrefix(annotation, annStep); isStep {
			if err := begin(strings.TrimSpace(name)); err != nil {
				return nil, err
			}

			continue
		}

		if current == nil {
			if err := begin(""); err != nil {
				return nil, err
			}
		}

		if err := apply(current, annotation); err != nil {
			return nil, err
		}
	}

	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("read migration contents: %w", err)
	}

	if err := flush(); err != nil {
		return nil, err
	}

	return steps, nil
}

// annotationOf returns the annotation body of a line, if it is one.
func annotationOf(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)

	body, ok := strings.CutPrefix(trimmed, strings.TrimSpace(prefix))
	if !ok {
		return "", false
	}

	return strings.TrimSpace(body), true
}

// apply records one annotation against the step being accumulated.
func apply(s *Step, annotation string) error {
	switch annotation {
	case annNoTx:
		s.Kind = step.KindDDLNoTx
		return nil

	case annNoLockTimeout:
		s.NoLockTimeout = true
		return nil
	}

	if expr, ok := strings.CutPrefix(annotation, annSatisfied); ok {
		return applySatisfied(s, strings.TrimSpace(expr))
	}

	return fmt.Errorf("%w %q", ErrUnknownAnnotation, annotation)
}

// applySatisfied records an author-supplied predicate.
func applySatisfied(s *Step, expr string) error {
	inner, ok := strings.CutPrefix(expr, sqlPredicatePrefix)
	if !ok || !strings.HasSuffix(inner, ")") {
		return fmt.Errorf("satisfied predicate %q must be of the form sql(SELECT ...)", expr)
	}

	s.Satisfied = strings.TrimSpace(strings.TrimSuffix(inner, ")"))

	if s.Satisfied == "" {
		return errors.New("satisfied predicate is empty")
	}

	return nil
}

// finish parses a step's accumulated SQL and completes its fields.
func finish(s *Step, body string) error {
	statements, err := parse.Parse(body)
	if err != nil {
		return fmt.Errorf("step %q: %w", s.Name, err)
	}

	if len(statements) == 0 {
		return fmt.Errorf("step %q: %w", s.Name, ErrEmptyStep)
	}

	checksum, err := parse.Checksum(body)
	if err != nil {
		return fmt.Errorf("step %q: %w", s.Name, err)
	}

	s.SQL = strings.TrimSpace(body)
	s.Statements = statements
	s.Checksum = checksum

	return nil
}
