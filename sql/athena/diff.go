// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package athena

import (
	"reflect"
	"strings"

	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/schema"
)

// normLocation returns a canonical form of an S3 LOCATION path by trimming a
// trailing slash. Athena's metastore drops trailing slashes on persistence,
// so this makes HCL-authored and inspected paths comparable.
func normLocation(p string) string {
	return strings.TrimRight(p, "/")
}

// DefaultDiff provides basic diffing capabilities for Athena dialects.
// Note, it is recommended to call Open, create a new Driver and use its
// Differ when a database connection is available.
var DefaultDiff schema.Differ = &sqlx.Diff{DiffDriver: &diff{}}

// A diff provides an Athena implementation for sqlx.DiffDriver.
type diff struct {
	*conn
}

// SchemaAttrDiff returns a changeset for migrating schema attributes from one state to the other.
func (*diff) SchemaAttrDiff(_, _ *schema.Schema) []schema.Change {
	return nil
}

// RealmObjectDiff returns a changeset for migrating realm (database) objects
// from one state to the other.
func (*diff) RealmObjectDiff(_, _ *schema.Realm) ([]schema.Change, error) {
	return nil, nil
}

// SchemaObjectDiff returns a changeset for migrating schema objects from
// one state to the other.
func (*diff) SchemaObjectDiff(_, _ *schema.Schema, _ *schema.DiffOptions) ([]schema.Change, error) {
	return nil, nil
}

// TableAttrDiff returns a changeset for migrating table attributes from one state to the other.
func (d *diff) TableAttrDiff(from, to *schema.Table, opts *schema.DiffOptions) ([]schema.Change, error) {
	var changes []schema.Change

	// Check for Location changes. Athena strips trailing slashes when it
	// persists a table's LOCATION, so an HCL-authored `s3://bucket/path/`
	// comes back as `s3://bucket/path` via DESCRIBE/SHOW CREATE TABLE.
	// Normalize before comparing so `apply` is idempotent.
	var fromLoc, toLoc Location
	if sqlx.Has(from.Attrs, &fromLoc) != sqlx.Has(to.Attrs, &toLoc) {
		if sqlx.Has(to.Attrs, &toLoc) {
			changes = append(changes, &schema.AddAttr{A: &toLoc})
		}
	} else if sqlx.Has(from.Attrs, &fromLoc) && sqlx.Has(to.Attrs, &toLoc) &&
		normLocation(fromLoc.Path) != normLocation(toLoc.Path) {
		changes = append(changes, &schema.ModifyAttr{From: &fromLoc, To: &toLoc})
	}

	// Check for StoredAs changes
	var fromFormat, toFormat StoredAs
	if sqlx.Has(from.Attrs, &fromFormat) != sqlx.Has(to.Attrs, &toFormat) {
		if sqlx.Has(to.Attrs, &toFormat) {
			changes = append(changes, &schema.AddAttr{A: &toFormat})
		}
	} else if sqlx.Has(from.Attrs, &fromFormat) && sqlx.Has(to.Attrs, &toFormat) && fromFormat.Format != toFormat.Format {
		changes = append(changes, &schema.ModifyAttr{From: &fromFormat, To: &toFormat})
	}

	// Check for TableProperties changes
	var fromProps, toProps TableProperties
	if sqlx.Has(from.Attrs, &fromProps) != sqlx.Has(to.Attrs, &toProps) {
		if sqlx.Has(to.Attrs, &toProps) {
			changes = append(changes, &schema.AddAttr{A: &toProps})
		}
	} else if sqlx.Has(from.Attrs, &fromProps) && sqlx.Has(to.Attrs, &toProps) {
		if !reflect.DeepEqual(fromProps.Properties, toProps.Properties) {
			changes = append(changes, &schema.ModifyAttr{From: &fromProps, To: &toProps})
		}
	}

	return append(changes, sqlx.CheckDiffMode(from, to, opts.Mode)...), nil
}

// ColumnChange returns the schema changes (if any) for migrating one column to the other.
func (d *diff) ColumnChange(_ *schema.Table, from, to *schema.Column, _ *schema.DiffOptions) (schema.Change, error) {
	var change schema.ChangeKind
	if from.Type.Null != to.Type.Null {
		change |= schema.ChangeNull
	}
	changed, err := d.typeChanged(from, to)
	if err != nil {
		return sqlx.NoChange, err
	}
	if changed {
		change |= schema.ChangeType
	}
	if d.commentChanged(from, to) {
		change |= schema.ChangeComment
	}
	if change.Is(schema.NoChange) {
		return sqlx.NoChange, nil
	}
	return &schema.ModifyColumn{
		Change: change,
		From:   from,
		To:     to,
	}, nil
}

// typeChanged reports if the column type was changed.
//
// We compare by the structural Go type only (and, for named types, by their
// identifying string) and intentionally do NOT compare Type.Raw: inspected
// Raw strings come from Athena (lowercase, possibly containing hive-specific
// punctuation such as `array<string>`) while HCL-loaded Raw strings come
// from the codec and may differ cosmetically even when the types are
// equivalent. Emitting a ModifyColumn for a Raw-only delta would produce a
// change Athena cannot apply (Hive tables do not support ALTER COLUMN TYPE)
// and break `schema apply` idempotency.
func (d *diff) typeChanged(from, to *schema.Column) (bool, error) {
	fromT, toT := from.Type.Type, to.Type.Type
	if fromT == nil || toT == nil {
		return fromT != toT, nil
	}
	if reflect.TypeOf(fromT) != reflect.TypeOf(toT) {
		return true, nil
	}
	return !sameType(fromT, toT), nil
}

// sameType reports whether two Athena column types are semantically
// equivalent. It inspects the well-known typed fields (T / Size / Precision
// / Scale / Unsigned, where applicable) instead of relying on the Raw string.
func sameType(a, b schema.Type) bool {
	switch av := a.(type) {
	case *schema.StringType:
		bv := b.(*schema.StringType)
		return av.T == bv.T && av.Size == bv.Size
	case *schema.IntegerType:
		bv := b.(*schema.IntegerType)
		return av.T == bv.T && av.Unsigned == bv.Unsigned
	case *schema.FloatType:
		bv := b.(*schema.FloatType)
		return av.T == bv.T && av.Precision == bv.Precision
	case *schema.DecimalType:
		bv := b.(*schema.DecimalType)
		return av.T == bv.T && av.Precision == bv.Precision && av.Scale == bv.Scale
	case *schema.BoolType:
		bv := b.(*schema.BoolType)
		return av.T == bv.T
	case *schema.BinaryType:
		bv := b.(*schema.BinaryType)
		return av.T == bv.T
	case *schema.TimeType:
		bv := b.(*schema.TimeType)
		return av.T == bv.T
	case *schema.UnsupportedType:
		bv := b.(*schema.UnsupportedType)
		return av.T == bv.T
	}
	// For any other Go type (including Athena-specific complex types) fall
	// back to reflect.DeepEqual. This is stricter than comparing Raw strings
	// but still avoids the Raw-only false positive.
	return reflect.DeepEqual(a, b)
}

// commentChanged reports if the column comment was changed.
func (*diff) commentChanged(from, to *schema.Column) bool {
	var c1, c2 schema.Comment
	return sqlx.Has(from.Attrs, &c1) != sqlx.Has(to.Attrs, &c2) || c1.Text != c2.Text
}

// IsGeneratedIndexName reports if the index name was generated by the database.
func (d *diff) IsGeneratedIndexName(_ *schema.Table, _ *schema.Index) bool {
	return false
}

// IndexAttrChanged reports if the index attributes were changed.
func (*diff) IndexAttrChanged(_, _ []schema.Attr) bool {
	return false
}

// IndexPartAttrChanged reports if the index-part attributes were changed.
func (*diff) IndexPartAttrChanged(_, _ *schema.Index, _ int) bool {
	return false
}

// ReferenceChanged reports if the foreign key referential action was changed.
func (*diff) ReferenceChanged(from, to schema.ReferenceOption) bool {
	return from != to
}

// ForeignKeyAttrChanged reports if any of the foreign-key attributes were changed.
func (*diff) ForeignKeyAttrChanged(_, _ []schema.Attr) bool {
	return false
}

// Normalize implements the sqlx.Normalizer interface.
func (d *diff) Normalize(_, _ *schema.Table, _ *schema.DiffOptions) error {
	return nil
}

// SupportChange reports if the change is supported by the differ.
func (*diff) SupportChange(c schema.Change) bool {
	switch c.(type) {
	// Athena has limited DDL support - only ADD COLUMNS is supported for regular tables
	case *schema.DropColumn, *schema.ModifyColumn, *schema.RenameColumn:
		return false
	case *schema.RenameTable:
		return false
	case *schema.RenameIndex, *schema.AddIndex, *schema.DropIndex, *schema.ModifyIndex:
		return false
	case *schema.AddForeignKey, *schema.DropForeignKey, *schema.ModifyForeignKey:
		return false
	case *schema.RenameConstraint:
		return false
	}
	return true
}
