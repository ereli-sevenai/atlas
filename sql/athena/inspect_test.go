// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package athena

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// realWorldCreateStmt is a real `SHOW CREATE TABLE` output from Athena,
// captured from an external JSON-SerDe table whose column types include
// nested structs with field names such as `deliverylocation` and
// `destinationlocation`. These substrings previously tricked the parser into
// treating them as the top-level LOCATION keyword.
const realWorldCreateStmt = "CREATE EXTERNAL TABLE `dev01_ingestion.dev01-defender`(\n" +
	"  `id` string COMMENT 'from deserializer', \n" +
	"  `messagesecuritystates` array<struct<deliveryaction:string,deliverylocation:string,recipientemail:string,messagesubject:string,messageid:string,messagecategory:string,senderip:string,sender:string,threats:array<struct<name:string,severity:string>>>> COMMENT 'from deserializer', \n" +
	"  `networkconnections` array<struct<applicationname:string,destinationaddress:string,destinationdomain:string,destinationlocation:string,destinationport:string>> COMMENT 'from deserializer')\n" +
	"PARTITIONED BY ( \n" +
	"  `tenant_id` string COMMENT '', \n" +
	"  `date` string COMMENT '', \n" +
	"  `source` string COMMENT '')\n" +
	"ROW FORMAT SERDE \n" +
	"  'org.openx.data.jsonserde.JsonSerDe' \n" +
	"WITH SERDEPROPERTIES ( \n" +
	"  'ignore.malformed.json'='true') \n" +
	"STORED AS INPUTFORMAT \n" +
	"  'org.apache.hadoop.mapred.TextInputFormat' \n" +
	"OUTPUTFORMAT \n" +
	"  'org.apache.hadoop.hive.ql.io.HiveIgnoreKeyTextOutputFormat'\n" +
	"LOCATION\n" +
	"  's3://duploservices-dev01-ingestion-014498642935/ingestion'\n" +
	"TBLPROPERTIES (\n" +
	"  'projection.date.format'='yyyy-MM-dd', \n" +
	"  'projection.date.range'='2024-06-28,NOW', \n" +
	"  'projection.date.type'='date', \n" +
	"  'projection.enabled'='true', \n" +
	"  'projection.source.type'='enum', \n" +
	"  'projection.source.values'='defender', \n" +
	"  'projection.tenant_id.type'='enum', \n" +
	"  'storage.location.template'='duploservices-dev01-ingestion-014498642935/ingestion/tenant_id=${tenant_id}/date=${date}/source=${source}/')\n"

func TestExtractLocation(t *testing.T) {
	for _, tt := range []struct {
		name string
		stmt string
		want string
	}{
		{
			name: "simple quoted location",
			stmt: "CREATE EXTERNAL TABLE `t`(`c` string)\nLOCATION 's3://bucket/path'\n",
			want: "s3://bucket/path",
		},
		{
			name: "location on its own line",
			stmt: "CREATE EXTERNAL TABLE `t`(`c` string)\nLOCATION\n  's3://bucket/path'\n",
			want: "s3://bucket/path",
		},
		{
			name: "column name contains LOCATION substring",
			stmt: "CREATE EXTERNAL TABLE `t`(`deliverylocation` string)\nLOCATION 's3://bucket/path'\n",
			want: "s3://bucket/path",
		},
		{
			name: "struct field contains location",
			stmt: "CREATE EXTERNAL TABLE `t`(`c` struct<deliverylocation:string,recipientemail:string>)\nLOCATION 's3://real/path'\n",
			want: "s3://real/path",
		},
		{
			name: "real-world defender table",
			stmt: realWorldCreateStmt,
			want: "s3://duploservices-dev01-ingestion-014498642935/ingestion",
		},
		{
			name: "missing location",
			stmt: "CREATE EXTERNAL TABLE `t`(`c` string)\n",
			want: "",
		},
		{
			name: "unquoted location takes until whitespace",
			stmt: "CREATE EXTERNAL TABLE `t`(`c` string)\nLOCATION s3://b/p\n",
			want: "s3://b/p",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, extractLocation(tt.stmt))
		})
	}
}

func TestExtractStoredAs(t *testing.T) {
	for _, tt := range []struct {
		name string
		stmt string
		want string
	}{
		{
			name: "simple parquet",
			stmt: "CREATE TABLE `t`(`c` string)\nSTORED AS PARQUET\nLOCATION 's3://x'",
			want: "PARQUET",
		},
		{
			name: "simple orc",
			stmt: "CREATE TABLE `t`(`c` string) STORED AS ORC LOCATION 's3://x'",
			want: "ORC",
		},
		{
			name: "inputformat text -> TEXTFILE",
			stmt: "CREATE TABLE `t`(`c` string)\nSTORED AS INPUTFORMAT\n  'org.apache.hadoop.mapred.TextInputFormat'\nOUTPUTFORMAT\n  'org.apache.hadoop.hive.ql.io.HiveIgnoreKeyTextOutputFormat'\nLOCATION 's3://x'",
			want: "TEXTFILE",
		},
		{
			name: "inputformat parquet -> PARQUET",
			stmt: "STORED AS INPUTFORMAT 'org.apache.hadoop.hive.ql.io.parquet.MapredParquetInputFormat' OUTPUTFORMAT 'org.apache.hadoop.hive.ql.io.parquet.MapredParquetOutputFormat'",
			want: "PARQUET",
		},
		{
			name: "inputformat unknown -> class name",
			stmt: "STORED AS INPUTFORMAT 'com.example.CustomInputFormat' OUTPUTFORMAT 'com.example.CustomOutputFormat'",
			want: "com.example.CustomInputFormat",
		},
		{
			name: "real-world defender table",
			stmt: realWorldCreateStmt,
			want: "TEXTFILE",
		},
		{
			name: "missing stored as",
			stmt: "CREATE TABLE `t`(`c` string)",
			want: "",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, extractStoredAs(tt.stmt))
		})
	}
}

func TestExtractTableProperties(t *testing.T) {
	t.Run("real-world defender table", func(t *testing.T) {
		got := extractTableProperties(realWorldCreateStmt)
		require.Equal(t, "yyyy-MM-dd", got["projection.date.format"])
		// Value contains a comma inside the string literal – must not be split.
		require.Equal(t, "2024-06-28,NOW", got["projection.date.range"])
		require.Equal(t, "defender", got["projection.source.values"])
		require.Equal(t, "true", got["projection.enabled"])
		require.Contains(t, got["storage.location.template"], "tenant_id=${tenant_id}")
	})
	t.Run("missing tblproperties", func(t *testing.T) {
		require.Nil(t, extractTableProperties("CREATE TABLE `t`(`c` string) LOCATION 's3://x'"))
	})
}

func TestFindTopLevelKeyword(t *testing.T) {
	for _, tt := range []struct {
		name    string
		stmt    string
		keyword string
		want    int // byte index of the found keyword, or -1
	}{
		{
			name:    "inside parens is ignored",
			stmt:    "CREATE (LOCATION inside) LOCATION outside",
			keyword: "LOCATION",
			want:    25,
		},
		{
			name:    "inside string literal is ignored",
			stmt:    "CREATE '...LOCATION...' LOCATION outside",
			keyword: "LOCATION",
			want:    24,
		},
		{
			name:    "word boundary required",
			stmt:    "deliverylocation LOCATION outside",
			keyword: "LOCATION",
			want:    17,
		},
		{
			name:    "multi-word keyword",
			stmt:    "CREATE TABLE (STORED AS inside) STORED AS PARQUET",
			keyword: "STORED AS",
			want:    32,
		},
		{
			name:    "not found",
			stmt:    "CREATE TABLE `t`(`c` string)",
			keyword: "LOCATION",
			want:    -1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, findTopLevelKeyword(tt.stmt, tt.keyword))
		})
	}
}

func TestIsWordBoundary(t *testing.T) {
	for _, c := range []byte{' ', '\t', '\n', '\r', '(', ')', ',', ';', '\''} {
		require.True(t, isWordBoundary(c), "%q should be a word boundary", c)
	}
	for _, c := range []byte{'a', 'Z', '0', '9', '_'} {
		require.False(t, isWordBoundary(c), "%q should not be a word boundary", c)
	}
}
