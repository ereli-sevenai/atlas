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

	if loc := extractLocation(stmt); loc != "" {
		t.AddAttrs(&Location{Path: loc})
	}

	if format := extractStoredAs(stmt); format != "" {
		t.AddAttrs(&StoredAs{Format: format})
	}

	if props := extractTableProperties(stmt); len(props) > 0 {
		t.AddAttrs(&TableProperties{Properties: props})
	}

	return nil
}

// findTopLevelKeyword searches stmt (case-insensitive) for `keyword` as a whole
// word at the top level of the statement – that is, outside of parenthesized
// groups (e.g. column lists, struct<...> types, TBLPROPERTIES bodies) and
// outside of single-quoted string literals. Returns the byte index in stmt or
// -1 if not found.
func findTopLevelKeyword(stmt, keyword string) int {
	ku := strings.ToUpper(keyword)
	depth := 0
	inStr := false
	for i := 0; i < len(stmt); i++ {
		c := stmt[i]
		if inStr {
			if c == '\'' {
				// Handle Hive's '' escape sequence.
				if i+1 < len(stmt) && stmt[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		switch c {
		case '\'':
			inStr = true
			continue
		case '(':
			depth++
			continue
		case ')':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 {
			continue
		}
		if i+len(ku) > len(stmt) {
			continue
		}
		if !strings.EqualFold(stmt[i:i+len(ku)], ku) {
			continue
		}
		leftOK := i == 0 || isWordBoundary(stmt[i-1])
		rightOK := i+len(ku) == len(stmt) || isWordBoundary(stmt[i+len(ku)])
		if leftOK && rightOK {
			return i
		}
	}
	return -1
}

func isWordBoundary(b byte) bool {
	return !((b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_')
}

// extractLocation returns the LOCATION value from a CREATE TABLE statement,
// or the empty string if not found.
func extractLocation(stmt string) string {
	idx := findTopLevelKeyword(stmt, "LOCATION")
	if idx == -1 {
		return ""
	}
	rest := strings.TrimSpace(stmt[idx+len("LOCATION"):])
	if len(rest) == 0 {
		return ""
	}
	if rest[0] == '\'' {
		if end := strings.Index(rest[1:], "'"); end != -1 {
			return rest[1 : end+1]
		}
		return ""
	}
	// Unquoted: take until whitespace.
	if end := strings.IndexAny(rest, " \t\n\r"); end != -1 {
		return rest[:end]
	}
	return rest
}

// extractStoredAs returns the STORED AS format from a CREATE TABLE statement.
// Handles both the simple form (e.g. "STORED AS PARQUET") and the more verbose
// form "STORED AS INPUTFORMAT '<class>' OUTPUTFORMAT '<class>'", in which case
// it infers a human-friendly format name from well-known INPUTFORMAT classes,
// falling back to the INPUTFORMAT class name itself.
func extractStoredAs(stmt string) string {
	idx := findTopLevelKeyword(stmt, "STORED AS")
	if idx == -1 {
		return ""
	}
	rest := strings.TrimSpace(stmt[idx+len("STORED AS"):])
	if rest == "" {
		return ""
	}
	// Detect the INPUTFORMAT / OUTPUTFORMAT form.
	if strings.HasPrefix(strings.ToUpper(rest), "INPUTFORMAT") {
		after := strings.TrimSpace(rest[len("INPUTFORMAT"):])
		if len(after) == 0 || after[0] != '\'' {
			return "INPUTFORMAT"
		}
		end := strings.Index(after[1:], "'")
		if end == -1 {
			return "INPUTFORMAT"
		}
		inputClass := after[1 : end+1]
		if f := inferFormat(inputClass); f != "" {
			return f
		}
		return inputClass
	}
	// Simple form: read the next word.
	if end := strings.IndexAny(rest, " \t\n\r("); end != -1 {
		return strings.TrimSpace(rest[:end])
	}
	return rest
}

// inferFormat maps well-known Hadoop/Hive INPUTFORMAT classes to simple
// Athena/Hive format keywords. Returns "" for unknown classes.
func inferFormat(inputFormat string) string {
	switch inputFormat {
	case "org.apache.hadoop.mapred.TextInputFormat":
		return "TEXTFILE"
	case "org.apache.hadoop.hive.ql.io.parquet.MapredParquetInputFormat":
		return "PARQUET"
	case "org.apache.hadoop.hive.ql.io.orc.OrcInputFormat":
		return "ORC"
	case "org.apache.hadoop.hive.ql.io.avro.AvroContainerInputFormat":
		return "AVRO"
	case "org.apache.hadoop.mapred.SequenceFileInputFormat":
		return "SEQUENCEFILE"
	case "org.apache.hadoop.hive.ql.io.RCFileInputFormat":
		return "RCFILE"
	case "org.apache.iceberg.mr.hive.HiveIcebergInputFormat":
		return "ICEBERG"
	}
	return ""
}

// extractTableProperties extracts TBLPROPERTIES from a CREATE TABLE statement.
func extractTableProperties(stmt string) map[string]string {
	idx := findTopLevelKeyword(stmt, "TBLPROPERTIES")
	if idx == -1 {
		return nil
	}
	rest := strings.TrimSpace(stmt[idx+len("TBLPROPERTIES"):])
	if len(rest) == 0 || rest[0] != '(' {
		return nil
	}
	// Find matching closing paren, honoring string literals.
	depth := 0
	inStr := false
	end := -1
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if inStr {
			if c == '\'' {
				if i+1 < len(rest) && rest[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		switch c {
		case '\'':
			inStr = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
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
	for _, pair := range splitTopLevelCommas(propsStr) {
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

// splitTopLevelCommas splits s on commas that are not inside a single-quoted
// string literal.
func splitTopLevelCommas(s string) []string {
	var out []string
	var b strings.Builder
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					b.WriteByte(s[i+1])
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		switch c {
		case '\'':
			inStr = true
			b.WriteByte(c)
		case ',':
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteByte(c)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
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
