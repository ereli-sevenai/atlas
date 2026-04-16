// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package athena

import (
	"context"
	"fmt"
	"strings"

	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"
)

// DefaultPlan provides basic planning capabilities for Athena dialects.
// Note, it is recommended to call Open, create a new Driver and use its
// migrate.PlanApplier when a database connection is available.
var DefaultPlan migrate.PlanApplier = &planApply{conn: &conn{ExecQuerier: sqlx.NoRows}}

// A planApply provides migration capabilities for schema elements.
type planApply struct{ *conn }

// PlanChanges returns a migration plan for the given schema changes.
func (p *planApply) PlanChanges(ctx context.Context, name string, changes []schema.Change, opts ...migrate.PlanOption) (*migrate.Plan, error) {
	s := &state{
		conn: p.conn,
		Plan: migrate.Plan{
			Name: name,
			// Athena does not support transactions
			Transactional: false,
		},
	}
	for _, o := range opts {
		o(&s.PlanOptions)
	}
	if err := s.plan(ctx, changes); err != nil {
		return nil, err
	}
	if err := sqlx.SetReversible(&s.Plan); err != nil {
		return nil, err
	}
	return &s.Plan, nil
}

// ApplyChanges applies the changes on the database. An error is returned
// if the driver is unable to produce a plan to it, or one of the statements
// is failed or unsupported.
func (p *planApply) ApplyChanges(ctx context.Context, changes []schema.Change, opts ...migrate.PlanOption) error {
	return sqlx.ApplyChanges(ctx, changes, p, opts...)
}

// state represents the state of a planning.
type state struct {
	*conn
	migrate.Plan
	migrate.PlanOptions
}

// plan generates migration plan for the given schema changes.
func (s *state) plan(_ context.Context, changes []schema.Change) (err error) {
	for _, c := range changes {
		switch c := c.(type) {
		case *schema.AddSchema:
			err = s.addSchema(c)
		case *schema.DropSchema:
			err = s.dropSchema(c)
		case *schema.AddTable:
			err = s.addTable(c)
		case *schema.DropTable:
			err = s.dropTable(c)
		case *schema.ModifyTable:
			err = s.modifyTable(c)
		default:
			err = fmt.Errorf("athena: unsupported change %T", c)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// addSchema builds the query for creating a database.
func (s *state) addSchema(add *schema.AddSchema) error {
	b := s.Build("CREATE DATABASE")
	if sqlx.Has(add.Extra, &schema.IfNotExists{}) {
		b.P("IF NOT EXISTS")
	}
	b.Ident(add.S.Name)
	s.append(&migrate.Change{
		Cmd:     b.String(),
		Source:  add,
		Reverse: s.Build("DROP DATABASE").Ident(add.S.Name).String(),
		Comment: fmt.Sprintf("create database %q", add.S.Name),
	})
	return nil
}

// dropSchema builds the query for dropping a database.
func (s *state) dropSchema(drop *schema.DropSchema) error {
	b := s.Build("DROP DATABASE")
	if sqlx.Has(drop.Extra, &schema.IfExists{}) {
		b.P("IF EXISTS")
	}
	b.Ident(drop.S.Name)
	s.append(&migrate.Change{
		Cmd:     b.String(),
		Source:  drop,
		Reverse: s.Build("CREATE DATABASE").Ident(drop.S.Name).String(),
		Comment: fmt.Sprintf("drop database %q", drop.S.Name),
	})
	return nil
}

// addTable builds and executes the query for creating a table.
func (s *state) addTable(add *schema.AddTable) error {
	var (
		errs     []string
		external = sqlx.Has(add.T.Attrs, &ExternalTable{})
		iceberg  = sqlx.Has(add.T.Attrs, &IcebergTable{})
	)

	b := s.Build("CREATE")
	if external {
		b.P("EXTERNAL")
	}
	b.P("TABLE")
	if sqlx.Has(add.Extra, &schema.IfNotExists{}) {
		b.P("IF NOT EXISTS")
	}
	b.Table(add.T)

	// Columns (partition columns are defined separately in PARTITIONED BY,
	// so filter them out before calling MapIndent — a skipped iteration
	// still emits a separator comma and would produce invalid DDL).
	nonPartCols := make([]*schema.Column, 0, len(add.T.Columns))
	for _, c := range add.T.Columns {
		if !isPartitionColumn(add.T, c.Name) {
			nonPartCols = append(nonPartCols, c)
		}
	}
	b.Wrap(func(b *sqlx.Builder) {
		b.MapIndent(nonPartCols, func(i int, b *sqlx.Builder) {
			c := nonPartCols[i]
			t, err := FormatType(c.Type.Type)
			if err != nil {
				errs = append(errs, err.Error())
				return
			}
			b.Ident(c.Name).P(t)
			if cmt := (&schema.Comment{}); sqlx.Has(c.Attrs, cmt) && cmt.Text != "" {
				b.P("COMMENT", fmt.Sprintf("'%s'", strings.ReplaceAll(cmt.Text, "'", "''")))
			}
		})
	})

	if len(errs) > 0 {
		return fmt.Errorf("athena: create table %q: %s", add.T.Name, strings.Join(errs, ", "))
	}

	// Partition columns. Emit COMMENT for each partition column so that
	// `schema apply` round-trips (inspect→apply) are idempotent – Athena
	// stores partition column comments and returns them via DESCRIBE, so
	// skipping COMMENT here produced a spurious ChangeComment on re-apply.
	if pc := (&PartitionColumns{}); sqlx.Has(add.T.Attrs, pc) && len(pc.Columns) > 0 {
		b.P("PARTITIONED BY").Wrap(func(b *sqlx.Builder) {
			for i, colName := range pc.Columns {
				if i > 0 {
					b.Comma()
				}
				if c, ok := add.T.Column(colName); ok {
					t, _ := FormatType(c.Type.Type)
					b.Ident(colName).P(t)
					if cmt := (&schema.Comment{}); sqlx.Has(c.Attrs, cmt) && cmt.Text != "" {
						b.P("COMMENT", fmt.Sprintf("'%s'", strings.ReplaceAll(cmt.Text, "'", "''")))
					}
				} else {
					b.Ident(colName).P("string")
				}
			}
		})
	}

	// Storage format
	if sf := (&StoredAs{}); sqlx.Has(add.T.Attrs, sf) && sf.Format != "" {
		// Handle INPUTFORMAT/OUTPUTFORMAT vs STORED AS
		b.P("STORED AS", sf.Format)
	}

	// Location (required for external tables)
	if loc := (&Location{}); sqlx.Has(add.T.Attrs, loc) && loc.Path != "" {
		b.P("LOCATION", fmt.Sprintf("'%s'", loc.Path))
	}

	// Table properties
	if props := (&TableProperties{}); sqlx.Has(add.T.Attrs, props) && len(props.Properties) > 0 {
		b.P("TBLPROPERTIES").Wrap(func(b *sqlx.Builder) {
			first := true
			for k, v := range props.Properties {
				if !first {
					b.Comma()
				}
				b.P(fmt.Sprintf("'%s'='%s'", k, v))
				first = false
			}
		})
	}

	// For Iceberg tables, add the table type property
	if iceberg {
		b.P("TBLPROPERTIES ('table_type'='ICEBERG')")
	}

	s.append(&migrate.Change{
		Cmd:     b.String(),
		Source:  add,
		Reverse: s.Build("DROP TABLE").Table(add.T).String(),
		Comment: fmt.Sprintf("create table %q", add.T.Name),
	})
	return nil
}

// dropTable builds the query for dropping a table.
func (s *state) dropTable(drop *schema.DropTable) error {
	b := s.Build("DROP TABLE")
	if sqlx.Has(drop.Extra, &schema.IfExists{}) {
		b.P("IF EXISTS")
	}
	b.Table(drop.T)
	s.append(&migrate.Change{
		Cmd:     b.String(),
		Source:  drop,
		Comment: fmt.Sprintf("drop table %q", drop.T.Name),
	})
	return nil
}

// modifyTable builds queries for modifying a table.
// Athena has very limited ALTER TABLE support.
func (s *state) modifyTable(modify *schema.ModifyTable) error {
	for _, change := range modify.Changes {
		switch c := change.(type) {
		case *schema.AddColumn:
			if err := s.addColumn(modify.T, c); err != nil {
				return err
			}
		case *schema.ModifyAttr:
			if err := s.modifyTableAttr(modify.T, c); err != nil {
				return err
			}
		default:
			return fmt.Errorf("athena: unsupported table modification %T (Athena has limited ALTER TABLE support)", c)
		}
	}
	return nil
}

// addColumn builds the query for adding a column.
func (s *state) addColumn(t *schema.Table, add *schema.AddColumn) error {
	typ, err := FormatType(add.C.Type.Type)
	if err != nil {
		return err
	}
	b := s.Build("ALTER TABLE").Table(t).P("ADD COLUMNS").Wrap(func(b *sqlx.Builder) {
		b.Ident(add.C.Name).P(typ)
		if c := (&schema.Comment{}); sqlx.Has(add.C.Attrs, c) && c.Text != "" {
			b.P("COMMENT", fmt.Sprintf("'%s'", strings.ReplaceAll(c.Text, "'", "''")))
		}
	})
	s.append(&migrate.Change{
		Cmd:     b.String(),
		Source:  add,
		Comment: fmt.Sprintf("add column %q to table %q", add.C.Name, t.Name),
	})
	return nil
}

// modifyTableAttr handles table attribute modifications.
func (s *state) modifyTableAttr(t *schema.Table, modify *schema.ModifyAttr) error {
	switch to := modify.To.(type) {
	case *Location:
		b := s.Build("ALTER TABLE").Table(t).P("SET LOCATION").P(fmt.Sprintf("'%s'", to.Path))
		s.append(&migrate.Change{
			Cmd:     b.String(),
			Source:  modify,
			Comment: fmt.Sprintf("set location for table %q", t.Name),
		})
	case *TableProperties:
		var props []string
		for k, v := range to.Properties {
			props = append(props, fmt.Sprintf("'%s'='%s'", k, v))
		}
		b := s.Build("ALTER TABLE").Table(t).P("SET TBLPROPERTIES").Wrap(func(b *sqlx.Builder) {
			b.P(strings.Join(props, ", "))
		})
		s.append(&migrate.Change{
			Cmd:     b.String(),
			Source:  modify,
			Comment: fmt.Sprintf("set table properties for %q", t.Name),
		})
	default:
		return fmt.Errorf("athena: unsupported table attribute modification %T", to)
	}
	return nil
}

func (s *state) append(c *migrate.Change) {
	s.Changes = append(s.Changes, c)
}

// Build instantiates a new builder and writes the given phrase to it.
func (s *state) Build(phrases ...string) *sqlx.Builder {
	return (*Driver).StmtBuilder(nil, s.PlanOptions).
		P(phrases...)
}

// isPartitionColumn checks if a column name is in the partition columns list.
func isPartitionColumn(t *schema.Table, name string) bool {
	pc := &PartitionColumns{}
	if !sqlx.Has(t.Attrs, pc) {
		return false
	}
	for _, c := range pc.Columns {
		if c == name {
			return true
		}
	}
	return false
}
