// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package athena

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/schema"
)

// A inspect provides an Athena implementation for schema.Inspector.
type inspect struct{ *conn }

var _ schema.Inspector = (*inspect)(nil)

// InspectRealm returns schema descriptions of all resources in the given realm.
func (i *inspect) InspectRealm(ctx context.Context, opts *schema.InspectRealmOption) (*schema.Realm, error) {
	schemas, err := i.databases(ctx, opts)
	if err != nil {
		return nil, err
	}
	if opts == nil {
		opts = &schema.InspectRealmOption{}
	}
	r := schema.NewRealm(schemas...)
	mode := sqlx.ModeInspectRealm(opts)
	if mode.Is(schema.InspectTables) {
		for _, s := range schemas {
			tables, err := i.tables(ctx, s.Name, nil)
			if err != nil {
				return nil, err
			}
			s.AddTables(tables...)
			for _, t := range tables {
				if err := i.inspectTable(ctx, t); err != nil {
					return nil, err
				}
			}
		}
		sqlx.LinkSchemaTables(r.Schemas)
	}
	return schema.ExcludeRealm(r, opts.Exclude)
}

// InspectSchema returns schema descriptions of the tables in the given schema.
func (i *inspect) InspectSchema(ctx context.Context, name string, opts *schema.InspectOptions) (*schema.Schema, error) {
	if name == "" {
		name = i.database
	}
	if name == "" {
		return nil, fmt.Errorf("athena: no database specified")
	}
	schemas, err := i.databases(ctx, &schema.InspectRealmOption{
		Schemas: []string{name},
	})
	if err != nil {
		return nil, err
	}
	if len(schemas) == 0 {
		return nil, &schema.NotExistError{
			Err: fmt.Errorf("athena: database %q was not found", name),
		}
	}
	if opts == nil {
		opts = &schema.InspectOptions{}
	}
	r := schema.NewRealm(schemas...)
	mode := sqlx.ModeInspectSchema(opts)
	if mode.Is(schema.InspectTables) {
		tables, err := i.tables(ctx, name, opts)
		if err != nil {
			return nil, err
		}
		r.Schemas[0].AddTables(tables...)
		for _, t := range tables {
			if err := i.inspectTable(ctx, t); err != nil {
				return nil, err
			}
		}
		sqlx.LinkSchemaTables(schemas)
	}
	return schema.ExcludeSchema(r.Schemas[0], opts.Exclude)
}

// databases returns the list of databases (schemas) in the Athena catalog.
func (i *inspect) databases(ctx context.Context, opts *schema.InspectRealmOption) ([]*schema.Schema, error) {
	rows, err := i.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, fmt.Errorf("athena: querying databases: %w", err)
	}
	defer rows.Close()

	var schemas []*schema.Schema
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("athena: scanning database name: %w", err)
		}
		if opts != nil && len(opts.Schemas) > 0 {
			found := false
			for _, s := range opts.Schemas {
				if s == name {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		schemas = append(schemas, &schema.Schema{Name: name})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("athena: iterating databases: %w", err)
	}
	return schemas, nil
}

// tables returns the list of tables in the given database.
func (i *inspect) tables(ctx context.Context, database string, opts *schema.InspectOptions) ([]*schema.Table, error) {
	query := fmt.Sprintf("SHOW TABLES IN `%s`", database)
	rows, err := i.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("athena: querying tables in %q: %w", database, err)
	}
	defer rows.Close()

	var tables []*schema.Table
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("athena: scanning table name: %w", err)
		}
		if opts != nil && len(opts.Tables) > 0 {
			found := false
			for _, t := range opts.Tables {
				if t == name {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		tables = append(tables, &schema.Table{Name: name})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("athena: iterating tables: %w", err)
	}
	return tables, nil
}

// inspectTable populates the table with columns and other metadata.
func (i *inspect) inspectTable(ctx context.Context, t *schema.Table) error {
	if err := i.columns(ctx, t); err != nil {
		return err
	}
	if err := i.tableProperties(ctx, t); err != nil {
		return err
	}
	return nil
}

// columns queries and appends the columns of the given table.
func (i *inspect) columns(ctx context.Context, t *schema.Table) error {
	database := i.database
	if t.Schema != nil && t.Schema.Name != "" {
		database = t.Schema.Name
	}

	query := fmt.Sprintf("DESCRIBE `%s`.`%s`", database, t.Name)
	rows, err := i.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("athena: describing table %q: %w", t.Name, err)
	}
	defer rows.Close()

	// Track partition columns separately
	inPartitionSection := false
	var partitionCols []string

	for rows.Next() {
		var colName, colType, comment sql.NullString
		if err := rows.Scan(&colName, &colType, &comment); err != nil {
			return fmt.Errorf("athena: scanning column: %w", err)
		}

		name := strings.TrimSpace(colName.String)
		typeName := strings.TrimSpace(colType.String)

		// Athena DESCRIBE output includes a separator line before partition columns
		if name == "" && typeName == "" {
			continue
		}
		if name == "# Partition Information" || strings.HasPrefix(name, "# ") {
			inPartitionSection = true
			continue
		}
		if inPartitionSection && (name == "# col_name" || typeName == "data_type") {
			continue
		}

		if inPartitionSection {
			partitionCols = append(partitionCols, name)
		}

		// Skip if column already exists (partition columns appear twice)
		if _, ok := t.Column(name); ok {
			continue
		}

		ct, err := ParseType(typeName)
		if err != nil {
			return fmt.Errorf("athena: parsing type %q for column %q: %w", typeName, name, err)
		}

		c := &schema.Column{
			Name: name,
			Type: &schema.ColumnType{
				Raw:  typeName,
				Type: ct,
				Null: true, // Athena columns are nullable by default
			},
		}
		if comment.Valid && comment.String != "" {
			c.AddAttrs(&schema.Comment{Text: comment.String})
		}
		t.Columns = append(t.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("athena: iterating columns: %w", err)
	}

	// Mark partition columns
	if len(partitionCols) > 0 {
		t.AddAttrs(&PartitionColumns{Columns: partitionCols})
	}

	return nil
}

// tableProperties extracts additional table properties like location and format.
func (i *inspect) tableProperties(ctx context.Context, t *schema.Table) error {
	database := i.database
	if t.Schema != nil && t.Schema.Name != "" {
		database = t.Schema.Name
	}

	query := fmt.Sprintf("SHOW CREATE TABLE `%s`.`%s`", database, t.Name)
	rows, err := i.QueryContext(ctx, query)
	if err != nil {
		// If SHOW CREATE TABLE fails, just skip properties
		return nil
	}
	defer rows.Close()

	var createStmt strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("athena: scanning create statement: %w", err)
		}
		createStmt.WriteString(line)
		createStmt.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("athena: iterating create statement: %w", err)
	}

	stmt := createStmt.String()
	t.AddAttrs(&CreateStmt{S: stmt})

	// Parse LOCATION
	if loc := extractProperty(stmt, "LOCATION"); loc != "" {
		t.AddAttrs(&Location{Path: loc})
	}

	// Parse STORED AS / ROW FORMAT
	if format := extractStoredAs(stmt); format != "" {
		t.AddAttrs(&StoredAs{Format: format})
	}

	// Parse TBLPROPERTIES
	if props := extractTableProperties(stmt); len(props) > 0 {
		t.AddAttrs(&TableProperties{Properties: props})
	}

	return nil
}

// extractProperty extracts a property value from a CREATE TABLE statement.
func extractProperty(stmt, prop string) string {
	upper := strings.ToUpper(stmt)
	idx := strings.Index(upper, prop)
	if idx == -1 {
		return ""
	}
	rest := stmt[idx+len(prop):]
	rest = strings.TrimSpace(rest)
	if len(rest) == 0 {
		return ""
	}
	// Extract quoted value
	if rest[0] == '\'' {
		end := strings.Index(rest[1:], "'")
		if end != -1 {
			return rest[1 : end+1]
		}
	}
	// Extract until whitespace or newline
	end := strings.IndexAny(rest, " \t\n\r")
	if end == -1 {
		return rest
	}
	return rest[:end]
}

// extractStoredAs extracts the storage format from a CREATE TABLE statement.
func extractStoredAs(stmt string) string {
	upper := strings.ToUpper(stmt)
	idx := strings.Index(upper, "STORED AS")
	if idx == -1 {
		return ""
	}
	rest := stmt[idx+len("STORED AS"):]
	rest = strings.TrimSpace(rest)
	// Get the format (e.g., PARQUET, ORC, TEXTFILE)
	end := strings.IndexAny(rest, " \t\n\r(")
	if end == -1 {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:end])
}

// extractTableProperties extracts TBLPROPERTIES from a CREATE TABLE statement.
func extractTableProperties(stmt string) map[string]string {
	upper := strings.ToUpper(stmt)
	idx := strings.Index(upper, "TBLPROPERTIES")
	if idx == -1 {
		return nil
	}
	rest := stmt[idx+len("TBLPROPERTIES"):]
	rest = strings.TrimSpace(rest)
	if len(rest) == 0 || rest[0] != '(' {
		return nil
	}
	// Find matching closing paren
	depth := 0
	end := -1
	for i, c := range rest {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
				break
			}
		}
		if end != -1 {
			break
		}
	}
	if end == -1 {
		return nil
	}
	propsStr := rest[1:end]
	props := make(map[string]string)
	// Parse key='value' pairs
	for _, pair := range strings.Split(propsStr, ",") {
		pair = strings.TrimSpace(pair)
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.Trim(strings.TrimSpace(parts[0]), "'\"")
		value := strings.Trim(strings.TrimSpace(parts[1]), "'\"")
		props[key] = value
	}
	return props
}

// Athena-specific table attributes

type (
	// CreateStmt describes the SQL statement used to create a resource.
	CreateStmt struct {
		schema.Attr
		S string
	}

	// Location describes the S3 location of the table data.
	Location struct {
		schema.Attr
		Path string
	}

	// StoredAs describes the storage format of the table.
	StoredAs struct {
		schema.Attr
		Format string
	}

	// TableProperties describes additional table properties.
	TableProperties struct {
		schema.Attr
		Properties map[string]string
	}

	// PartitionColumns describes the partition columns of the table.
	PartitionColumns struct {
		schema.Attr
		Columns []string
	}

	// ExternalTable indicates that the table is an external table.
	ExternalTable struct {
		schema.Attr
	}

	// IcebergTable indicates that the table is an Iceberg table.
	IcebergTable struct {
		schema.Attr
	}
)
