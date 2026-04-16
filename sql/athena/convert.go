// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package athena

import (
	"fmt"

	"ariga.io/atlas/schemahcl"
	"ariga.io/atlas/sql/internal/specutil"
	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/schema"
	"ariga.io/atlas/sql/sqlspec"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
)

type doc struct {
	Tables  []*sqlspec.Table  `spec:"table"`
	Schemas []*sqlspec.Schema `spec:"schema"`
}

// Codec for schemahcl.
type Codec struct {
	State *schemahcl.State
}

// Eval evaluates an Atlas DDL document into v using the input.
func (c *Codec) Eval(p *hclparse.Parser, v any, input map[string]cty.Value) error {
	return c.EvalOptions(p, v, &schemahcl.EvalOptions{Variables: input})
}

// EvalOptions decodes the HCL with the given options.
func (c *Codec) EvalOptions(p *hclparse.Parser, v any, opts *schemahcl.EvalOptions) error {
	switch v := v.(type) {
	case *schema.Realm:
		var d doc
		if err := c.State.EvalOptions(p, &d, opts); err != nil {
			return err
		}
		if err := specutil.Scan(v,
			&specutil.ScanDoc{Schemas: d.Schemas, Tables: d.Tables},
			scanFuncs,
		); err != nil {
			return fmt.Errorf("athena: failed converting to *schema.Realm: %w", err)
		}
	case *schema.Schema:
		var d doc
		if err := c.State.EvalOptions(p, &d, opts); err != nil {
			return err
		}
		if len(d.Schemas) != 1 {
			return fmt.Errorf("athena: expecting document to contain a single schema, got %d", len(d.Schemas))
		}
		r := &schema.Realm{}
		if err := specutil.Scan(r,
			&specutil.ScanDoc{Schemas: d.Schemas, Tables: d.Tables},
			scanFuncs,
		); err != nil {
			return err
		}
		*v = *r.Schemas[0]
	case schema.Schema, schema.Realm:
		return fmt.Errorf("athena: Eval expects a pointer: received %[1]T, expected *%[1]T", v)
	default:
		return fmt.Errorf("athena: unexpected type %T", v)
	}
	return nil
}

// MarshalSpec marshals v into an Atlas DDL document using a schemahcl.Marshaler.
func (c *Codec) MarshalSpec(v any) ([]byte, error) {
	return specutil.Marshal(v, c.State, specutil.RealmFuncs{
		Schema: schemaSpec,
	})
}

var scanFuncs = &specutil.ScanFuncs{
	Table: convertTable,
}

// convertTable converts a sqlspec.Table to a schema.Table.
func convertTable(spec *sqlspec.Table, parent *schema.Schema) (*schema.Table, error) {
	t, err := specutil.Table(spec, parent, convertColumn, specutil.PrimaryKey, convertIndex, specutil.Check)
	if err != nil {
		return nil, err
	}

	// Handle external table attribute
	if attr, ok := spec.Attr("external"); ok {
		b, err := attr.Bool()
		if err != nil {
			return nil, err
		}
		if b {
			t.AddAttrs(&ExternalTable{})
		}
	}

	// Handle iceberg table attribute
	if attr, ok := spec.Attr("iceberg"); ok {
		b, err := attr.Bool()
		if err != nil {
			return nil, err
		}
		if b {
			t.AddAttrs(&IcebergTable{})
		}
	}

	// Handle location attribute
	if attr, ok := spec.Attr("location"); ok {
		loc, err := attr.String()
		if err != nil {
			return nil, err
		}
		t.AddAttrs(&Location{Path: loc})
	}

	// Handle stored_as attribute
	if attr, ok := spec.Attr("stored_as"); ok {
		format, err := attr.String()
		if err != nil {
			return nil, err
		}
		t.AddAttrs(&StoredAs{Format: format})
	}

	// Handle partition_columns attribute
	if attr, ok := spec.Attr("partition_columns"); ok {
		cols, err := attr.Strings()
		if err != nil {
			return nil, err
		}
		t.AddAttrs(&PartitionColumns{Columns: cols})
	}

	return t, nil
}

// convertColumn converts a sqlspec.Column into a schema.Column.
func convertColumn(spec *sqlspec.Column, _ *schema.Table) (*schema.Column, error) {
	return specutil.Column(spec, convertColumnType)
}

// convertIndex wraps specutil.Index to satisfy the non-variadic
// specutil.ConvertIndexFunc signature expected by specutil.Table.
func convertIndex(spec *sqlspec.Index, t *schema.Table) (*schema.Index, error) {
	return specutil.Index(spec, t)
}

// indexSpec wraps specutil.FromIndex to satisfy the non-variadic
// specutil.IndexSpecFunc signature expected by specutil.FromTable.
func indexSpec(idx *schema.Index) (*sqlspec.Index, error) {
	return specutil.FromIndex(idx)
}

// convertColumnType converts a sqlspec.Column into a concrete Athena schema.Type.
func convertColumnType(spec *sqlspec.Column) (schema.Type, error) {
	return TypeRegistry.Type(spec.Type, spec.Extra.Attrs)
}

// schemaSpec converts from a concrete Athena schema to Atlas specification.
func schemaSpec(s *schema.Schema) (*specutil.SchemaSpec, error) {
	return specutil.FromSchema(s, &specutil.SchemaFuncs{
		Table: tableSpec,
	})
}

// tableSpec converts from a concrete Athena schema.Table to a sqlspec.Table.
func tableSpec(t *schema.Table) (*sqlspec.Table, error) {
	spec, err := specutil.FromTable(
		t,
		columnSpec,
		specutil.FromPrimaryKey,
		indexSpec,
		specutil.FromForeignKey,
		specutil.FromCheck,
	)
	if err != nil {
		return nil, err
	}

	// Add external attribute
	if sqlx.Has(t.Attrs, &ExternalTable{}) {
		spec.Extra.Attrs = append(spec.Extra.Attrs, schemahcl.BoolAttr("external", true))
	}

	// Add iceberg attribute
	if sqlx.Has(t.Attrs, &IcebergTable{}) {
		spec.Extra.Attrs = append(spec.Extra.Attrs, schemahcl.BoolAttr("iceberg", true))
	}

	// Add location attribute
	if loc := (&Location{}); sqlx.Has(t.Attrs, loc) && loc.Path != "" {
		spec.Extra.Attrs = append(spec.Extra.Attrs, schemahcl.StringAttr("location", loc.Path))
	}

	// Add stored_as attribute
	if sf := (&StoredAs{}); sqlx.Has(t.Attrs, sf) && sf.Format != "" {
		spec.Extra.Attrs = append(spec.Extra.Attrs, schemahcl.StringAttr("stored_as", sf.Format))
	}

	// Add partition_columns attribute
	if pc := (&PartitionColumns{}); sqlx.Has(t.Attrs, pc) && len(pc.Columns) > 0 {
		spec.Extra.Attrs = append(spec.Extra.Attrs, schemahcl.StringsAttr("partition_columns", pc.Columns...))
	}

	return spec, nil
}

// columnSpec converts from a concrete Athena schema.Column into a sqlspec.Column.
func columnSpec(c *schema.Column, _ *schema.Table) (*sqlspec.Column, error) {
	return specutil.FromColumn(c, columnTypeSpec)
}

// columnTypeSpec converts from a concrete Athena schema.Type into sqlspec.Column Type.
func columnTypeSpec(t schema.Type) (*sqlspec.Column, error) {
	st, err := TypeRegistry.Convert(t)
	if err != nil {
		return nil, err
	}
	return &sqlspec.Column{Type: st}, nil
}

// TypeRegistry contains the supported TypeSpecs for the Athena driver.
var TypeRegistry = schemahcl.NewRegistry(
	schemahcl.WithFormatter(FormatType),
	schemahcl.WithParser(ParseType),
	schemahcl.WithSpecs(
		// Boolean
		schemahcl.NewTypeSpec(TypeBoolean),

		// Integer types
		schemahcl.NewTypeSpec(TypeTinyInt),
		schemahcl.NewTypeSpec(TypeSmallInt),
		schemahcl.NewTypeSpec(TypeInt),
		schemahcl.NewTypeSpec(TypeInteger),
		schemahcl.NewTypeSpec(TypeBigInt),

		// Floating point types
		schemahcl.NewTypeSpec(TypeDouble),
		schemahcl.NewTypeSpec(TypeFloat),
		schemahcl.NewTypeSpec(TypeReal),
		schemahcl.NewTypeSpec(TypeDecimal, schemahcl.WithAttributes(schemahcl.PrecisionTypeAttr(), schemahcl.ScaleTypeAttr())),

		// String types
		schemahcl.NewTypeSpec(TypeChar, schemahcl.WithAttributes(schemahcl.SizeTypeAttr(false))),
		schemahcl.NewTypeSpec(TypeVarchar, schemahcl.WithAttributes(schemahcl.SizeTypeAttr(false))),
		schemahcl.NewTypeSpec(TypeString),

		// Binary types
		schemahcl.NewTypeSpec(TypeBinary),
		schemahcl.NewTypeSpec(TypeVarbinary),

		// Date/Time types
		schemahcl.NewTypeSpec(TypeDate),
		schemahcl.NewTypeSpec(TypeTimestamp),

		// JSON
		schemahcl.NewTypeSpec(TypeJSON),

		// Complex types (represented as strings in HCL)
		schemahcl.NewTypeSpec(TypeArray),
		schemahcl.NewTypeSpec(TypeMap),
		schemahcl.NewTypeSpec(TypeStruct),
	),
)

var (
	codec = &Codec{
		State: schemahcl.New(append(
			specOptions,
			schemahcl.WithTypes("table.column.type", TypeRegistry.Specs()),
			schemahcl.WithScopedEnums("table.foreign_key.on_update", specutil.ReferenceVars...),
			schemahcl.WithScopedEnums("table.foreign_key.on_delete", specutil.ReferenceVars...),
		)...),
	}
	// MarshalHCL marshals v into an Atlas HCL DDL document.
	MarshalHCL = schemahcl.MarshalerFunc(codec.MarshalSpec)
	// EvalHCL implements the schemahcl.Evaluator interface.
	EvalHCL = schemahcl.EvalFunc(codec.Eval)
	// EvalHCLBytes is a helper that evaluates an HCL document from a byte slice instead
	// of from an hclparse.Parser instance.
	EvalHCLBytes = specutil.HCLBytesFunc(EvalHCL)
)
