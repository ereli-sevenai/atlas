// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package athena

import (
	"testing"

	"ariga.io/atlas/sql/schema"
	"github.com/stretchr/testify/require"
)

func TestParseType(t *testing.T) {
	tests := []struct {
		input    string
		expected schema.Type
	}{
		// Boolean
		{input: "boolean", expected: &schema.BoolType{T: "boolean"}},
		{input: "bool", expected: &schema.BoolType{T: "boolean"}},

		// Integer types
		{input: "tinyint", expected: &schema.IntegerType{T: "tinyint"}},
		{input: "smallint", expected: &schema.IntegerType{T: "smallint"}},
		{input: "int", expected: &schema.IntegerType{T: "int"}},
		{input: "integer", expected: &schema.IntegerType{T: "integer"}},
		{input: "bigint", expected: &schema.IntegerType{T: "bigint"}},

		// Floating point types
		{input: "double", expected: &schema.FloatType{T: "double"}},
		{input: "float", expected: &schema.FloatType{T: "float"}},
		{input: "real", expected: &schema.FloatType{T: "real"}},

		// Decimal types
		{input: "decimal", expected: &schema.DecimalType{T: "decimal"}},
		{input: "decimal(10)", expected: &schema.DecimalType{T: "decimal", Precision: 10}},
		{input: "decimal(10,2)", expected: &schema.DecimalType{T: "decimal", Precision: 10, Scale: 2}},

		// String types
		{input: "string", expected: &schema.StringType{T: "string"}},
		{input: "char", expected: &schema.StringType{T: "char"}},
		{input: "char(10)", expected: &schema.StringType{T: "char", Size: 10}},
		{input: "varchar", expected: &schema.StringType{T: "varchar"}},
		{input: "varchar(255)", expected: &schema.StringType{T: "varchar", Size: 255}},

		// Binary types
		{input: "binary", expected: &schema.BinaryType{T: "binary"}},
		{input: "varbinary", expected: &schema.BinaryType{T: "varbinary"}},

		// Date/Time types
		{input: "date", expected: &schema.TimeType{T: "date"}},
		{input: "timestamp", expected: &schema.TimeType{T: "timestamp"}},

		// JSON
		{input: "json", expected: &schema.JSONType{T: "json"}},

		// Complex types
		{input: "array<string>", expected: &ArrayType{T: "string"}},
		{input: "array<int>", expected: &ArrayType{T: "int"}},
		{input: "map<string,int>", expected: &MapType{KeyType: "string", ValueType: "int"}},
		{input: "struct<name:string,age:int>", expected: &StructType{
			Fields: []StructField{
				{Name: "name", Type: "string"},
				{Name: "age", Type: "int"},
			},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := ParseType(tt.input)
			require.NoError(t, err)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestFormatType(t *testing.T) {
	tests := []struct {
		input    schema.Type
		expected string
	}{
		// Boolean
		{input: &schema.BoolType{T: "boolean"}, expected: "boolean"},

		// Integer types
		{input: &schema.IntegerType{T: "tinyint"}, expected: "tinyint"},
		{input: &schema.IntegerType{T: "int"}, expected: "int"},
		{input: &schema.IntegerType{T: "bigint"}, expected: "bigint"},

		// Floating point types
		{input: &schema.FloatType{T: "double"}, expected: "double"},
		{input: &schema.FloatType{T: "float"}, expected: "float"},

		// Decimal types
		{input: &schema.DecimalType{T: "decimal"}, expected: "decimal"},
		{input: &schema.DecimalType{T: "decimal", Precision: 10}, expected: "decimal(10)"},
		{input: &schema.DecimalType{T: "decimal", Precision: 10, Scale: 2}, expected: "decimal(10,2)"},

		// String types
		{input: &schema.StringType{T: "string"}, expected: "string"},
		{input: &schema.StringType{T: "char"}, expected: "char"},
		{input: &schema.StringType{T: "char", Size: 10}, expected: "char(10)"},
		{input: &schema.StringType{T: "varchar"}, expected: "varchar"},
		{input: &schema.StringType{T: "varchar", Size: 255}, expected: "varchar(255)"},

		// Binary types
		{input: &schema.BinaryType{T: "binary"}, expected: "binary"},
		{input: &schema.BinaryType{T: "varbinary"}, expected: "varbinary"},

		// Date/Time types
		{input: &schema.TimeType{T: "date"}, expected: "date"},
		{input: &schema.TimeType{T: "timestamp"}, expected: "timestamp"},

		// JSON
		{input: &schema.JSONType{T: "json"}, expected: "json"},

		// Complex types
		{input: &ArrayType{T: "string"}, expected: "array<string>"},
		{input: &MapType{KeyType: "string", ValueType: "int"}, expected: "map<string,int>"},
		{input: &StructType{
			Fields: []StructField{
				{Name: "name", Type: "string"},
				{Name: "age", Type: "int"},
			},
		}, expected: "struct<name:string,age:int>"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result, err := FormatType(tt.input)
			require.NoError(t, err)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestSplitTopLevel(t *testing.T) {
	tests := []struct {
		input    string
		delim    rune
		expected []string
	}{
		{input: "a,b,c", delim: ',', expected: []string{"a", "b", "c"}},
		{input: "string,int", delim: ',', expected: []string{"string", "int"}},
		{input: "array<string>,int", delim: ',', expected: []string{"array<string>", "int"}},
		{input: "map<string,int>,boolean", delim: ',', expected: []string{"map<string,int>", "boolean"}},
		{input: "struct<a:int,b:string>,array<map<string,int>>", delim: ',', expected: []string{"struct<a:int,b:string>", "array<map<string,int>>"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := splitTopLevel(tt.input, tt.delim)
			require.Equal(t, tt.expected, result)
		})
	}
}
