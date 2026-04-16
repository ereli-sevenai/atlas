// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package athena

import (
	"fmt"
	"strconv"
	"strings"

	"ariga.io/atlas/sql/schema"
)

// Athena data types as defined in AWS documentation.
// https://docs.aws.amazon.com/athena/latest/ug/data-types.html
const (
	TypeBoolean = "boolean"

	TypeTinyInt  = "tinyint"
	TypeSmallInt = "smallint"
	TypeInt      = "int"
	TypeInteger  = "integer"
	TypeBigInt   = "bigint"

	TypeDouble  = "double"
	TypeFloat   = "float"
	TypeReal    = "real"
	TypeDecimal = "decimal"

	TypeChar    = "char"
	TypeVarchar = "varchar"
	TypeString  = "string"

	TypeBinary    = "binary"
	TypeVarbinary = "varbinary"

	TypeDate      = "date"
	TypeTimestamp = "timestamp"

	TypeArray  = "array"
	TypeMap    = "map"
	TypeStruct = "struct"

	TypeJSON = "json"
)

type (
	// ArrayType represents an Athena ARRAY type.
	ArrayType struct {
		schema.Type
		T string // The element type.
	}

	// MapType represents an Athena MAP type.
	MapType struct {
		schema.Type
		KeyType   string
		ValueType string
	}

	// StructType represents an Athena STRUCT type.
	StructType struct {
		schema.Type
		Fields []StructField
	}

	// StructField represents a field in an Athena STRUCT.
	StructField struct {
		Name string
		Type string
	}
)

// FormatType converts a schema type to its Athena column form.
func FormatType(t schema.Type) (string, error) {
	switch t := t.(type) {
	case *schema.BoolType:
		return TypeBoolean, nil
	case *schema.IntegerType:
		return strings.ToLower(t.T), nil
	case *schema.FloatType:
		return strings.ToLower(t.T), nil
	case *schema.DecimalType:
		if t.Precision > 0 {
			if t.Scale > 0 {
				return fmt.Sprintf("decimal(%d,%d)", t.Precision, t.Scale), nil
			}
			return fmt.Sprintf("decimal(%d)", t.Precision), nil
		}
		return TypeDecimal, nil
	case *schema.StringType:
		switch strings.ToLower(t.T) {
		case TypeChar:
			if t.Size > 0 {
				return fmt.Sprintf("char(%d)", t.Size), nil
			}
			return TypeChar, nil
		case TypeVarchar:
			if t.Size > 0 {
				return fmt.Sprintf("varchar(%d)", t.Size), nil
			}
			return TypeVarchar, nil
		default:
			return strings.ToLower(t.T), nil
		}
	case *schema.BinaryType:
		return strings.ToLower(t.T), nil
	case *schema.TimeType:
		return strings.ToLower(t.T), nil
	case *schema.JSONType:
		return TypeJSON, nil
	case *ArrayType:
		return fmt.Sprintf("array<%s>", t.T), nil
	case *MapType:
		return fmt.Sprintf("map<%s,%s>", t.KeyType, t.ValueType), nil
	case *StructType:
		var fields []string
		for _, f := range t.Fields {
			fields = append(fields, fmt.Sprintf("%s:%s", f.Name, f.Type))
		}
		return fmt.Sprintf("struct<%s>", strings.Join(fields, ",")), nil
	case *schema.UnsupportedType:
		return "", fmt.Errorf("athena: unsupported type: %q", t.T)
	default:
		return "", fmt.Errorf("athena: invalid schema type: %T", t)
	}
}

// ParseType returns the schema.Type value represented by the given raw type.
func ParseType(s string) (schema.Type, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return &schema.StringType{T: TypeString}, nil
	}

	// Handle complex types first
	if strings.HasPrefix(s, "array<") {
		inner := strings.TrimSuffix(strings.TrimPrefix(s, "array<"), ">")
		return &ArrayType{T: inner}, nil
	}
	if strings.HasPrefix(s, "map<") {
		inner := strings.TrimSuffix(strings.TrimPrefix(s, "map<"), ">")
		parts := splitTopLevel(inner, ',')
		if len(parts) != 2 {
			return nil, fmt.Errorf("athena: invalid map type: %s", s)
		}
		return &MapType{KeyType: strings.TrimSpace(parts[0]), ValueType: strings.TrimSpace(parts[1])}, nil
	}
	if strings.HasPrefix(s, "struct<") {
		inner := strings.TrimSuffix(strings.TrimPrefix(s, "struct<"), ">")
		var fields []StructField
		for _, f := range splitTopLevel(inner, ',') {
			parts := strings.SplitN(strings.TrimSpace(f), ":", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("athena: invalid struct field: %s", f)
			}
			fields = append(fields, StructField{Name: strings.TrimSpace(parts[0]), Type: strings.TrimSpace(parts[1])})
		}
		return &StructType{Fields: fields}, nil
	}

	// Handle parameterized types
	base, params := parseTypeParams(s)
	switch base {
	case TypeBoolean, "bool":
		return &schema.BoolType{T: TypeBoolean}, nil
	case TypeTinyInt, TypeSmallInt, TypeInt, TypeInteger, TypeBigInt:
		return &schema.IntegerType{T: base}, nil
	case TypeDouble, TypeFloat, TypeReal:
		return &schema.FloatType{T: base}, nil
	case TypeDecimal:
		dt := &schema.DecimalType{T: TypeDecimal}
		if len(params) > 0 {
			p, err := strconv.Atoi(params[0])
			if err != nil {
				return nil, fmt.Errorf("athena: invalid decimal precision: %s", params[0])
			}
			dt.Precision = p
		}
		if len(params) > 1 {
			s, err := strconv.Atoi(params[1])
			if err != nil {
				return nil, fmt.Errorf("athena: invalid decimal scale: %s", params[1])
			}
			dt.Scale = s
		}
		return dt, nil
	case TypeChar:
		st := &schema.StringType{T: TypeChar}
		if len(params) > 0 {
			size, err := strconv.Atoi(params[0])
			if err != nil {
				return nil, fmt.Errorf("athena: invalid char size: %s", params[0])
			}
			st.Size = size
		}
		return st, nil
	case TypeVarchar:
		st := &schema.StringType{T: TypeVarchar}
		if len(params) > 0 {
			size, err := strconv.Atoi(params[0])
			if err != nil {
				return nil, fmt.Errorf("athena: invalid varchar size: %s", params[0])
			}
			st.Size = size
		}
		return st, nil
	case TypeString:
		return &schema.StringType{T: TypeString}, nil
	case TypeBinary, TypeVarbinary:
		return &schema.BinaryType{T: base}, nil
	case TypeDate, TypeTimestamp:
		return &schema.TimeType{T: base}, nil
	case TypeJSON:
		return &schema.JSONType{T: TypeJSON}, nil
	default:
		return &schema.UnsupportedType{T: s}, nil
	}
}

// parseTypeParams parses a type string like "decimal(10,2)" into base type and parameters.
func parseTypeParams(s string) (string, []string) {
	idx := strings.Index(s, "(")
	if idx == -1 {
		return s, nil
	}
	base := s[:idx]
	paramsStr := strings.TrimSuffix(strings.TrimPrefix(s[idx:], "("), ")")
	params := strings.Split(paramsStr, ",")
	for i := range params {
		params[i] = strings.TrimSpace(params[i])
	}
	return base, params
}

// splitTopLevel splits a string by a delimiter, but only at the top level
// (not inside nested angle brackets).
func splitTopLevel(s string, delim rune) []string {
	var result []string
	var current strings.Builder
	depth := 0
	for _, c := range s {
		switch c {
		case '<':
			depth++
			current.WriteRune(c)
		case '>':
			depth--
			current.WriteRune(c)
		case delim:
			if depth == 0 {
				result = append(result, current.String())
				current.Reset()
			} else {
				current.WriteRune(c)
			}
		default:
			current.WriteRune(c)
		}
	}
	if current.Len() > 0 {
		result = append(result, current.String())
	}
	return result
}
