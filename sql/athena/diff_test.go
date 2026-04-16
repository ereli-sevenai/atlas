// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package athena

import (
	"testing"

	"ariga.io/atlas/sql/schema"
	"github.com/stretchr/testify/require"
)

func TestDiff_SupportChange(t *testing.T) {
	d := &diff{}

	// Supported changes
	require.True(t, d.SupportChange(&schema.AddColumn{}))
	require.True(t, d.SupportChange(&schema.AddTable{}))
	require.True(t, d.SupportChange(&schema.DropTable{}))
	require.True(t, d.SupportChange(&schema.AddSchema{}))
	require.True(t, d.SupportChange(&schema.DropSchema{}))

	// Unsupported changes (Athena limitations)
	require.False(t, d.SupportChange(&schema.DropColumn{}))
	require.False(t, d.SupportChange(&schema.ModifyColumn{}))
	require.False(t, d.SupportChange(&schema.RenameColumn{}))
	require.False(t, d.SupportChange(&schema.RenameTable{}))
	require.False(t, d.SupportChange(&schema.AddIndex{}))
	require.False(t, d.SupportChange(&schema.DropIndex{}))
	require.False(t, d.SupportChange(&schema.ModifyIndex{}))
	require.False(t, d.SupportChange(&schema.RenameIndex{}))
	require.False(t, d.SupportChange(&schema.AddForeignKey{}))
	require.False(t, d.SupportChange(&schema.DropForeignKey{}))
	require.False(t, d.SupportChange(&schema.ModifyForeignKey{}))
}

func TestDiff_TableAttrDiff(t *testing.T) {
	d := &diff{}

	// Test Location change
	from := &schema.Table{
		Name:  "test",
		Attrs: []schema.Attr{&Location{Path: "s3://bucket/old/"}},
	}
	to := &schema.Table{
		Name:  "test",
		Attrs: []schema.Attr{&Location{Path: "s3://bucket/new/"}},
	}

	changes, err := d.TableAttrDiff(from, to, &schema.DiffOptions{})
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.IsType(t, &schema.ModifyAttr{}, changes[0])
	modifyAttr := changes[0].(*schema.ModifyAttr)
	require.Equal(t, "s3://bucket/old/", modifyAttr.From.(*Location).Path)
	require.Equal(t, "s3://bucket/new/", modifyAttr.To.(*Location).Path)

	// Test no change
	from2 := &schema.Table{
		Name:  "test",
		Attrs: []schema.Attr{&Location{Path: "s3://bucket/path/"}},
	}
	to2 := &schema.Table{
		Name:  "test",
		Attrs: []schema.Attr{&Location{Path: "s3://bucket/path/"}},
	}

	changes2, err := d.TableAttrDiff(from2, to2, &schema.DiffOptions{})
	require.NoError(t, err)
	require.Empty(t, changes2)

	// Trailing slash normalization: Athena stores LOCATION without a
	// trailing slash, so HCL-authored `s3://bucket/path/` must diff as
	// equivalent to the inspected `s3://bucket/path`.
	from3 := &schema.Table{
		Name:  "test",
		Attrs: []schema.Attr{&Location{Path: "s3://bucket/path"}},
	}
	to3 := &schema.Table{
		Name:  "test",
		Attrs: []schema.Attr{&Location{Path: "s3://bucket/path/"}},
	}
	changes3, err := d.TableAttrDiff(from3, to3, &schema.DiffOptions{})
	require.NoError(t, err)
	require.Empty(t, changes3, "trailing slash should not produce a diff")
}

func TestDiff_ColumnChange_RawOnlyNoDiff(t *testing.T) {
	d := &diff{}
	// Inspect sets Raw to Athena's lowercase reply ("string") while HCL
	// may leave it empty. Structurally equivalent types must NOT produce a
	// ModifyColumn – otherwise round-trip (apply → inspect → apply) would
	// loop forever on Athena, which cannot ALTER COLUMN TYPE.
	from := &schema.Column{
		Name: "col",
		Type: &schema.ColumnType{
			Raw:  "string",
			Type: &schema.StringType{T: "string"},
			Null: true,
		},
	}
	to := &schema.Column{
		Name: "col",
		Type: &schema.ColumnType{
			Raw:  "",
			Type: &schema.StringType{T: "string"},
			Null: true,
		},
	}
	change, err := d.ColumnChange(nil, from, to, &schema.DiffOptions{})
	require.NoError(t, err)
	require.Nil(t, change, "expected no-change when only Raw differs")
}

func TestDiff_ColumnChange(t *testing.T) {
	d := &diff{}

	// Test type change
	from := &schema.Column{
		Name: "col1",
		Type: &schema.ColumnType{
			Raw:  "int",
			Type: &schema.IntegerType{T: "int"},
			Null: false,
		},
	}
	to := &schema.Column{
		Name: "col1",
		Type: &schema.ColumnType{
			Raw:  "bigint",
			Type: &schema.IntegerType{T: "bigint"},
			Null: false,
		},
	}

	change, err := d.ColumnChange(nil, from, to, &schema.DiffOptions{})
	require.NoError(t, err)
	require.NotNil(t, change)
	modifyCol := change.(*schema.ModifyColumn)
	require.True(t, modifyCol.Change.Is(schema.ChangeType))

	// Test null change
	from2 := &schema.Column{
		Name: "col1",
		Type: &schema.ColumnType{
			Raw:  "int",
			Type: &schema.IntegerType{T: "int"},
			Null: false,
		},
	}
	to2 := &schema.Column{
		Name: "col1",
		Type: &schema.ColumnType{
			Raw:  "int",
			Type: &schema.IntegerType{T: "int"},
			Null: true,
		},
	}

	change2, err := d.ColumnChange(nil, from2, to2, &schema.DiffOptions{})
	require.NoError(t, err)
	require.NotNil(t, change2)
	modifyCol2 := change2.(*schema.ModifyColumn)
	require.True(t, modifyCol2.Change.Is(schema.ChangeNull))
}
