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

	pgquery "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// constNode is the message a literal is carried in, named through the
// generated type rather than spelled, so a change in the grammar's shape is a
// compile error instead of a rule that quietly stops matching.
var constNode = (&pgquery.A_Const{}).ProtoReflect().Descriptor().FullName()

// stringNode is the message an identifier part is carried in, named the same
// way and for the same reason.
var stringNode = (&pgquery.String{}).ProtoReflect().Descriptor().FullName()

// l024 flags an enum value used in the transaction that added it.
//
// Postgres refuses it: the new label is not visible to the transaction that
// created it, and the statement fails with "unsafe use of new value". The
// step fails at run time; the linter fails it at review time.
type l024 struct{}

func (l024) ID() string { return L024 }

func (l024) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	if !transactional(ctx) {
		return nil
	}

	var findings []Finding

	// Only labels added earlier in this same step are a hazard for this
	// statement; one added later is a different problem, and not this one.
	for _, earlier := range ctx.Step.Parsed[:ctx.StmtIndex] {
		label := earlier.GetStmt().GetAlterEnumStmt().GetNewVal()
		if label == "" || !mentions(stmt, label) {
			continue
		}

		findings = append(findings, finding(SeverityError, fmt.Sprintf(
			"%q was added to its type earlier in this transaction, and Postgres will "+
				"refuse to use it here; add the value in a step of its own",
			label), ctx)...)
	}

	return findings
}

// mentions reports whether the statement carries the label as a literal
// anywhere in its tree, which is how a new enum value is written wherever it
// is used: in a value list, in a comparison, in a cast.
func mentions(stmt *pgquery.RawStmt, label string) bool {
	found := false

	walk(stmt.ProtoReflect(), func(message protoreflect.Message) {
		if message.Descriptor().FullName() != constNode {
			return
		}

		if literal := message.Interface().(*pgquery.A_Const); literal.GetSval().GetSval() == label {
			found = true
		}
	})

	return found
}

// mentionsIdent reports whether the SQL names the identifier anywhere in its
// tree, which is how a predicate is read for the column it should have
// consulted. The statement is parsed rather than searched as text: a name in
// a comment or inside a literal is not a reference to it.
func mentionsIdent(sql, name string) bool {
	found := false

	// A predicate the loader accepted parses; one it did not is not this
	// rule's finding, and an empty tree mentions nothing.
	tree, _ := pgquery.Parse(sql)

	walk(tree.ProtoReflect(), func(message protoreflect.Message) {
		if message.Descriptor().FullName() != stringNode {
			return
		}

		if text, ok := message.Interface().(*pgquery.String); ok && text.GetSval() == name {
			found = true
		}
	})

	return found
}

// walk visits every message in a parse tree, the tree itself included.
func walk(message protoreflect.Message, visit func(protoreflect.Message)) {
	visit(message)

	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsList():
			list := value.List()

			for i := range list.Len() {
				if field.Kind() == protoreflect.MessageKind {
					walk(list.Get(i).Message(), visit)
				}
			}

		case field.Kind() == protoreflect.MessageKind:
			walk(value.Message(), visit)
		}

		return true
	})
}
