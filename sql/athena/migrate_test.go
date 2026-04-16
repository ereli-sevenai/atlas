// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package athena

import (
	"context"
	"testing"

	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"
	"github.com/stretchr/testify/require"
)

func TestPlanChanges_AddSchema(t *testing.T) {
	p := &planApply{conn: &conn{}}
	plan, err := p.PlanChanges(context.Background(), "test", []schema.Change{
		&schema.AddSchema{S: &schema.Schema{Name: "my_database"}},
	})
	require.NoError(t, err)
	require.Len(t, plan.Changes, 1)
	require.Equal(t, "CREATE DATABASE `my_database`", plan.Changes[0].Cmd)
	require.Equal(t, "DROP DATABASE `my_database`", plan.Changes[0].Reverse)
}

func TestPlanChanges_DropSchema(t *testing.T) {
	p := &planApply{conn: &conn{}}
	plan, err := p.PlanChanges(context.Background(), "test", []schema.Change{
		&schema.DropSchema{S: &schema.Schema{Name: "my_database"}},
	})
	require.NoError(t, err)
	require.Len(t, plan.Changes, 1)
	require.Equal(t, "DROP DATABASE `my_database`", plan.Changes[0].Cmd)
	require.Equal(t, "CREATE DATABASE `my_database`", plan.Changes[0].Reverse)
}

func TestPlanChanges_AddTable(t *testing.T) {
	p := &planApply{conn: &conn{}}

	// Simple table
	table := &schema.Table{
		Name: "users",
		Columns: []*schema.Column{
			{Name: "id", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "bigint"}}},
			{Name: "name", Type: &schema.ColumnType{Type: &schema.StringType{T: "string"}}},
		},
	}

	plan, err := p.PlanChanges(context.Background(), "test", []schema.Change{
		&schema.AddTable{T: table},
	})
	require.NoError(t, err)
	require.Len(t, plan.Changes, 1)
	require.Contains(t, plan.Changes[0].Cmd, "CREATE TABLE")
	require.Contains(t, plan.Changes[0].Cmd, "`users`")
	require.Contains(t, plan.Changes[0].Cmd, "`id` bigint")
	require.Contains(t, plan.Changes[0].Cmd, "`name` string")
}

func TestPlanChanges_AddExternalTable(t *testing.T) {
	p := &planApply{conn: &conn{}}

	table := &schema.Table{
		Name: "events",
		Columns: []*schema.Column{
			{Name: "event_id", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "bigint"}}},
			{Name: "event_type", Type: &schema.ColumnType{Type: &schema.StringType{T: "string"}}},
		},
		Attrs: []schema.Attr{
			&ExternalTable{},
			&Location{Path: "s3://my-bucket/events/"},
			&StoredAs{Format: "PARQUET"},
		},
	}

	plan, err := p.PlanChanges(context.Background(), "test", []schema.Change{
		&schema.AddTable{T: table},
	})
	require.NoError(t, err)
	require.Len(t, plan.Changes, 1)
	require.Contains(t, plan.Changes[0].Cmd, "CREATE EXTERNAL TABLE")
	require.Contains(t, plan.Changes[0].Cmd, "STORED AS PARQUET")
	require.Contains(t, plan.Changes[0].Cmd, "LOCATION 's3://my-bucket/events/'")
}

func TestPlanChanges_AddPartitionedTable(t *testing.T) {
	p := &planApply{conn: &conn{}}

	table := &schema.Table{
		Name: "logs",
		Columns: []*schema.Column{
			{Name: "log_id", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "bigint"}}},
			{Name: "message", Type: &schema.ColumnType{Type: &schema.StringType{T: "string"}}},
			{Name: "dt", Type: &schema.ColumnType{Type: &schema.StringType{T: "string"}}},
		},
		Attrs: []schema.Attr{
			&ExternalTable{},
			&Location{Path: "s3://my-bucket/logs/"},
			&StoredAs{Format: "PARQUET"},
			&PartitionColumns{Columns: []string{"dt"}},
		},
	}

	plan, err := p.PlanChanges(context.Background(), "test", []schema.Change{
		&schema.AddTable{T: table},
	})
	require.NoError(t, err)
	require.Len(t, plan.Changes, 1)
	require.Contains(t, plan.Changes[0].Cmd, "PARTITIONED BY")
	require.Contains(t, plan.Changes[0].Cmd, "`dt`")
}

func TestPlanChanges_DropTable(t *testing.T) {
	p := &planApply{conn: &conn{}}
	plan, err := p.PlanChanges(context.Background(), "test", []schema.Change{
		&schema.DropTable{T: &schema.Table{Name: "users"}},
	})
	require.NoError(t, err)
	require.Len(t, plan.Changes, 1)
	require.Equal(t, "DROP TABLE `users`", plan.Changes[0].Cmd)
}

func TestPlanChanges_AddColumn(t *testing.T) {
	p := &planApply{conn: &conn{}}

	table := &schema.Table{Name: "users"}
	column := &schema.Column{
		Name: "email",
		Type: &schema.ColumnType{Type: &schema.StringType{T: "string"}},
	}

	plan, err := p.PlanChanges(context.Background(), "test", []schema.Change{
		&schema.ModifyTable{
			T: table,
			Changes: []schema.Change{
				&schema.AddColumn{C: column},
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, plan.Changes, 1)
	require.Contains(t, plan.Changes[0].Cmd, "ALTER TABLE `users` ADD COLUMNS")
	require.Contains(t, plan.Changes[0].Cmd, "`email` string")
}

func TestPlanChanges_ModifyLocation(t *testing.T) {
	p := &planApply{conn: &conn{}}

	table := &schema.Table{Name: "users"}

	plan, err := p.PlanChanges(context.Background(), "test", []schema.Change{
		&schema.ModifyTable{
			T: table,
			Changes: []schema.Change{
				&schema.ModifyAttr{
					From: &Location{Path: "s3://bucket/old/"},
					To:   &Location{Path: "s3://bucket/new/"},
				},
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, plan.Changes, 1)
	require.Contains(t, plan.Changes[0].Cmd, "ALTER TABLE `users` SET LOCATION 's3://bucket/new/'")
}

func TestPlanChanges_UnsupportedChange(t *testing.T) {
	p := &planApply{conn: &conn{}}

	table := &schema.Table{Name: "users"}
	column := &schema.Column{
		Name: "name",
		Type: &schema.ColumnType{Type: &schema.StringType{T: "string"}},
	}

	// DropColumn is not supported in Athena
	_, err := p.PlanChanges(context.Background(), "test", []schema.Change{
		&schema.ModifyTable{
			T: table,
			Changes: []schema.Change{
				&schema.DropColumn{C: column},
			},
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported")
}

func TestPlanChanges_NotTransactional(t *testing.T) {
	p := &planApply{conn: &conn{}}
	plan, err := p.PlanChanges(context.Background(), "test", []schema.Change{
		&schema.AddSchema{S: &schema.Schema{Name: "my_database"}},
	})
	require.NoError(t, err)
	// Athena does not support transactions
	require.False(t, plan.Transactional)
}

func TestPlanOptions(t *testing.T) {
	p := &planApply{conn: &conn{}}

	// Test with schema qualifier
	qualifier := "my_db"
	plan, err := p.PlanChanges(context.Background(), "test", []schema.Change{
		&schema.AddTable{T: &schema.Table{
			Name: "users",
			Columns: []*schema.Column{
				{Name: "id", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "int"}}},
			},
		}},
	}, func(o *migrate.PlanOptions) {
		o.SchemaQualifier = &qualifier
	})
	require.NoError(t, err)
	require.Len(t, plan.Changes, 1)
}
