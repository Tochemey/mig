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

// Package lockmodel predicts the table locks a statement takes: which
// relations, in which mode, and held for how long.
//
// The mode alone is not the hazard. ACCESS EXCLUSIVE held for a catalog
// update is routine; ACCESS EXCLUSIVE held for a table rewrite is an outage.
// Every prediction therefore pairs a mode with a duration class.
//
// The prediction is offline: it reads the parse tree and nothing else, so a
// relation only the catalog knows about, such as a dependent view, an inbound
// foreign key or an index rebuilt by a rewrite, is not in it. The prediction
// is checked against a live server, lock by lock, in test/lockmatrix.
package lockmodel

// The option names Postgres attaches to a statement's parse tree. They are
// exported because the rule catalog reads the same trees: VACUUM FULL is the
// lock model's business and L010's alike.
const (
	OptionFull         = "full"
	OptionConcurrently = "concurrently"
)

const (
	// Instant is catalog-only work, held for microseconds.
	Instant DurationClass = iota + 1

	// Scan is one full read of the rows.
	Scan

	// Rewrite is a full copy of the table: O(rows) writes plus the disk for a
	// second copy.
	Rewrite

	// IndexBuild is O(rows log rows), and a concurrent build additionally
	// waits out every open transaction, twice.
	IndexBuild
)

// durationNames maps each class to its rendered form.
var durationNames = map[DurationClass]string{
	Instant:    "instant",
	Scan:       "scan",
	Rewrite:    "rewrite",
	IndexBuild: "index build",
}

// LockMode is a Postgres table-level lock mode. The numeric values follow the
// server's own lock levels, weakest to strongest, so ordering comparisons and
// LOCK TABLE's parse tree agree with it.
type LockMode int

const (
	// AccessShare is taken by SELECT.
	AccessShare LockMode = iota + 1

	// RowShare is taken by SELECT ... FOR UPDATE and its variants.
	RowShare

	// RowExclusive is taken by INSERT, UPDATE and DELETE.
	RowExclusive

	// ShareUpdateExclusive is taken by CREATE INDEX CONCURRENTLY, VALIDATE
	// CONSTRAINT and VACUUM. It blocks DDL and other maintenance, not traffic.
	ShareUpdateExclusive

	// Share is taken by CREATE INDEX without CONCURRENTLY.
	Share

	// ShareRowExclusive is taken by ADD FOREIGN KEY, on both tables.
	ShareRowExclusive

	// Exclusive is taken by REFRESH MATERIALIZED VIEW CONCURRENTLY.
	Exclusive

	// AccessExclusive is taken by most ALTER TABLE forms, DROP and TRUNCATE.
	AccessExclusive
)

// modeNames maps each mode to its pg_locks spelling. The human form is the
// same name without the Lock suffix, spaced at the case boundaries.
var modeNames = map[LockMode]string{
	AccessShare:          "AccessShareLock",
	RowShare:             "RowShareLock",
	RowExclusive:         "RowExclusiveLock",
	ShareUpdateExclusive: "ShareUpdateExclusiveLock",
	Share:                "ShareLock",
	ShareRowExclusive:    "ShareRowExclusiveLock",
	Exclusive:            "ExclusiveLock",
	AccessExclusive:      "AccessExclusiveLock",
}

// humanNames maps each mode to the spelling the SQL grammar and the
// documentation use.
var humanNames = map[LockMode]string{
	AccessShare:          "ACCESS SHARE",
	RowShare:             "ROW SHARE",
	RowExclusive:         "ROW EXCLUSIVE",
	ShareUpdateExclusive: "SHARE UPDATE EXCLUSIVE",
	Share:                "SHARE",
	ShareRowExclusive:    "SHARE ROW EXCLUSIVE",
	Exclusive:            "EXCLUSIVE",
	AccessExclusive:      "ACCESS EXCLUSIVE",
}

// String renders the mode as the SQL grammar spells it.
func (m LockMode) String() string {
	if name, ok := humanNames[m]; ok {
		return name
	}

	return "UNKNOWN"
}

// PgLocksName renders the mode as pg_locks spells it.
func (m LockMode) PgLocksName() string {
	return modeNames[m]
}

// ModeFromPgLocks reads a pg_locks mode spelling back into a mode.
func ModeFromPgLocks(name string) (LockMode, bool) {
	for mode, spelling := range modeNames {
		if spelling == name {
			return mode, true
		}
	}

	return 0, false
}

// BlocksReads reports whether holding the mode stops plain SELECT. Only
// ACCESS EXCLUSIVE conflicts with ACCESS SHARE.
func (m LockMode) BlocksReads() bool {
	return m == AccessExclusive
}

// BlocksWrites reports whether holding the mode stops INSERT, UPDATE and
// DELETE, which is every mode conflicting with ROW EXCLUSIVE.
func (m LockMode) BlocksWrites() bool {
	return m >= Share
}

// DurationClass says how long a lock is held, as a function of the table
// rather than of the moment.
type DurationClass int

// String renders the duration class.
func (d DurationClass) String() string {
	if name, ok := durationNames[d]; ok {
		return name
	}

	return "unknown"
}

// Relation names a table, index, view or materialised view. Schema is empty
// when the statement did not qualify the name, in which case the server
// resolves it through the search path.
type Relation struct {
	Schema string
	Name   string
}

// String renders the name, schema-qualified when the statement was.
func (r Relation) String() string {
	if r.Schema == "" {
		return r.Name
	}

	return r.Schema + "." + r.Name
}

// LockEffect is one lock the statement takes.
type LockEffect struct {
	Relation Relation
	Mode     LockMode
	Duration DurationClass

	// Implicit marks a lock the statement's author did not spell out, such as
	// the referenced side of a foreign key.
	Implicit bool

	// Reason says why the lock is taken and why it lasts as long as it does.
	Reason string
}

// BlockProfile says what traffic the statement stops while its locks are held.
type BlockProfile struct {
	Reads  bool
	Writes bool
}

// Analysis is the predicted locking behaviour of one statement.
type Analysis struct {
	Effects []LockEffect

	// NoTx reports a statement the server refuses inside a transaction block,
	// which is what forces a migration step to run with notx.
	NoTx bool
}

// Blocks folds the effects into what the statement blocks.
func (a Analysis) Blocks() BlockProfile {
	profile := BlockProfile{}

	for _, effect := range a.Effects {
		profile.Reads = profile.Reads || effect.Mode.BlocksReads()
		profile.Writes = profile.Writes || effect.Mode.BlocksWrites()
	}

	return profile
}
