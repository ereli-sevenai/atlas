// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package athena

import (
	"net/url"
	"os"
	"testing"

	"ariga.io/atlas/sql/migrate"
	"github.com/stretchr/testify/require"
)

// parseDSNValues parses an athenadriver DSN (s3://bucket/path?k=v&...) into
// its scheme+host+path prefix and decoded query values for easy assertions.
func parseDSNValues(t *testing.T, dsn string) (string, url.Values) {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	return u.Scheme + "://" + u.Host + u.Path, u.Query()
}

func TestParseURL_SchemaAndDSN(t *testing.T) {
	p := parser{}
	u, err := url.Parse("athena://AKID:SECRET@athena.us-east-1.amazonaws.com/mydb?s3_staging_dir=s3://bucket/path")
	require.NoError(t, err)
	got := p.ParseURL(u)
	require.Equal(t, "mydb", got.Schema)
	prefix, q := parseDSNValues(t, got.DSN)
	require.Equal(t, "s3://bucket/path", prefix)
	require.Equal(t, "us-east-1", q.Get("region"))
	require.Equal(t, "mydb", q.Get("db"))
	require.Equal(t, "AKID", q.Get("accessID"))
	require.Equal(t, "SECRET", q.Get("secretAccessKey"))
	require.Empty(t, q.Get("sessionToken"))
}

func TestParseURL_AuthModes(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		assert func(t *testing.T, q url.Values)
	}{
		{
			name:  "static credentials",
			input: "athena://AKID:SECRET@athena.us-east-1.amazonaws.com/mydb?s3_staging_dir=s3://bucket/path",
			assert: func(t *testing.T, q url.Values) {
				require.Equal(t, "AKID", q.Get("accessID"))
				require.Equal(t, "SECRET", q.Get("secretAccessKey"))
				require.Empty(t, q.Get("sessionToken"))
				require.Empty(t, q.Get("AWSProfile"))
			},
		},
		{
			name:  "static credentials with session token",
			input: "athena://AKID:SECRET@athena.us-west-2.amazonaws.com/mydb?s3_staging_dir=s3://bucket/path&session_token=FQoDYXdz",
			assert: func(t *testing.T, q url.Values) {
				require.Equal(t, "AKID", q.Get("accessID"))
				require.Equal(t, "SECRET", q.Get("secretAccessKey"))
				require.Equal(t, "FQoDYXdz", q.Get("sessionToken"))
				require.Equal(t, "us-west-2", q.Get("region"))
			},
		},
		{
			name:  "default credential chain",
			input: "athena://athena.eu-west-1.amazonaws.com/mydb?s3_staging_dir=s3://bucket/path",
			assert: func(t *testing.T, q url.Values) {
				require.Empty(t, q.Get("accessID"))
				require.Empty(t, q.Get("secretAccessKey"))
				require.Empty(t, q.Get("AWSProfile"))
				require.Equal(t, "eu-west-1", q.Get("region"))
			},
		},
		{
			name:  "named profile",
			input: "athena://athena.us-east-1.amazonaws.com/mydb?s3_staging_dir=s3://bucket/path&profile=production",
			assert: func(t *testing.T, q url.Values) {
				require.Equal(t, "production", q.Get("AWSProfile"))
				require.Empty(t, q.Get("accessID"))
			},
		},
		{
			name:  "assume role",
			input: "athena://athena.us-east-1.amazonaws.com/mydb?s3_staging_dir=s3://bucket/path&role_arn=arn:aws:iam::123:role/atlas&role_session_name=atlas",
			assert: func(t *testing.T, q url.Values) {
				// role_arn is applied via env vars, not the DSN.
				require.Empty(t, q.Get("role_arn"))
				require.Empty(t, q.Get("accessID"))
			},
		},
		{
			name:  "web identity (OIDC)",
			input: "athena://athena.us-east-1.amazonaws.com/mydb?s3_staging_dir=s3://bucket/path&role_arn=arn:aws:iam::123:role/atlas&web_identity_token_file=/var/run/secrets/token",
			assert: func(t *testing.T, q url.Values) {
				require.Empty(t, q.Get("accessID"))
				require.Empty(t, q.Get("web_identity_token_file"))
			},
		},
		{
			name:  "explicit region overrides host",
			input: "athena://athena.us-east-1.amazonaws.com/mydb?s3_staging_dir=s3://bucket/path&region=eu-central-1",
			assert: func(t *testing.T, q url.Values) {
				require.Equal(t, "eu-central-1", q.Get("region"))
			},
		},
		{
			name:  "workgroup passthrough",
			input: "athena://athena.us-east-1.amazonaws.com/mydb?s3_staging_dir=s3://bucket/path&workgroup=primary",
			assert: func(t *testing.T, q url.Values) {
				require.Equal(t, "primary", q.Get("workgroupName"))
			},
		},
	}
	p := parser{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.input)
			require.NoError(t, err)
			got := p.ParseURL(u)
			_, q := parseDSNValues(t, got.DSN)
			tt.assert(t, q)
		})
	}
}

func TestParseURL_Errors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "missing s3_staging_dir",
			input:   "athena://AKID:SECRET@athena.us-east-1.amazonaws.com/mydb",
			wantErr: "s3_staging_dir",
		},
		{
			name:    "unknown region in host, no region param",
			input:   "athena://AKID:SECRET@example.com/mydb?s3_staging_dir=s3://bucket/path",
			wantErr: "region",
		},
		{
			name:    "invalid s3_staging_dir scheme",
			input:   "athena://AKID:SECRET@athena.us-east-1.amazonaws.com/mydb?s3_staging_dir=https://bucket/path",
			wantErr: "s3_staging_dir",
		},
	}
	p := parser{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.input)
			require.NoError(t, err)
			got := p.ParseURL(u)
			require.Contains(t, got.DSN, "err=")
			require.Contains(t, got.DSN, tt.wantErr)
		})
	}
}

func TestRegionFromHost(t *testing.T) {
	tests := map[string]string{
		"athena.us-east-1.amazonaws.com":    "us-east-1",
		"athena.eu-central-1.amazonaws.com": "eu-central-1",
		"athena.ap-southeast-2.amazonaws.com": "ap-southeast-2",
		"athena.cn-north-1.amazonaws.com.cn": "cn-north-1",
		"example.com":                       "",
		"athena.amazonaws.com":              "",
	}
	for host, want := range tests {
		t.Run(host, func(t *testing.T) {
			require.Equal(t, want, regionFromHost(host))
		})
	}
}

func TestApplyAuthEnv_Profile(t *testing.T) {
	t.Setenv("AWS_SDK_LOAD_CONFIG", "")
	t.Setenv("AWS_PROFILE", "")
	u, err := url.Parse("athena://athena.us-east-1.amazonaws.com/db?s3_staging_dir=s3://bucket/p&profile=dev")
	require.NoError(t, err)
	require.NoError(t, applyAuthEnv(u))
	require.Equal(t, "1", mustGetenv(t, "AWS_SDK_LOAD_CONFIG"))
	require.Equal(t, "dev", mustGetenv(t, "AWS_PROFILE"))
}

func TestApplyAuthEnv_AssumeRole(t *testing.T) {
	t.Setenv("AWS_ROLE_ARN", "")
	t.Setenv("AWS_ROLE_SESSION_NAME", "")
	u, err := url.Parse("athena://athena.us-east-1.amazonaws.com/db?s3_staging_dir=s3://bucket/p&role_arn=arn:aws:iam::123:role/atlas&role_session_name=atlas-cli")
	require.NoError(t, err)
	require.NoError(t, applyAuthEnv(u))
	require.Equal(t, "arn:aws:iam::123:role/atlas", mustGetenv(t, "AWS_ROLE_ARN"))
	require.Equal(t, "atlas-cli", mustGetenv(t, "AWS_ROLE_SESSION_NAME"))
}

func TestApplyAuthEnv_WebIdentity(t *testing.T) {
	t.Setenv("AWS_ROLE_ARN", "")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	u, err := url.Parse("athena://athena.us-east-1.amazonaws.com/db?s3_staging_dir=s3://bucket/p&role_arn=arn:aws:iam::123:role/atlas&web_identity_token_file=/tmp/tok")
	require.NoError(t, err)
	require.NoError(t, applyAuthEnv(u))
	require.Equal(t, "arn:aws:iam::123:role/atlas", mustGetenv(t, "AWS_ROLE_ARN"))
	require.Equal(t, "/tmp/tok", mustGetenv(t, "AWS_WEB_IDENTITY_TOKEN_FILE"))
}

func TestApplyAuthEnv_WebIdentityRequiresRoleARN(t *testing.T) {
	t.Setenv("AWS_ROLE_ARN", "")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	u, err := url.Parse("athena://athena.us-east-1.amazonaws.com/db?s3_staging_dir=s3://bucket/p&web_identity_token_file=/tmp/tok")
	require.NoError(t, err)
	err = applyAuthEnv(u)
	require.Error(t, err)
	require.Contains(t, err.Error(), "role_arn")
}

func TestApplyAuthEnv_ExternalIDRejected(t *testing.T) {
	t.Setenv("AWS_ROLE_ARN", "")
	u, err := url.Parse("athena://athena.us-east-1.amazonaws.com/db?s3_staging_dir=s3://bucket/p&role_arn=arn:aws:iam::123:role/atlas&external_id=abc")
	require.NoError(t, err)
	err = applyAuthEnv(u)
	require.Error(t, err)
	require.Contains(t, err.Error(), "external_id")
}

func TestApplyAuthEnv_DoesNotOverrideExisting(t *testing.T) {
	t.Setenv("AWS_PROFILE", "already-set")
	u, err := url.Parse("athena://athena.us-east-1.amazonaws.com/db?s3_staging_dir=s3://bucket/p&profile=should-not-apply")
	require.NoError(t, err)
	require.NoError(t, applyAuthEnv(u))
	require.Equal(t, "already-set", mustGetenv(t, "AWS_PROFILE"))
}

func mustGetenv(t *testing.T, k string) string {
	t.Helper()
	v, ok := os.LookupEnv(k)
	require.True(t, ok, "env var %q not set", k)
	return v
}

func TestChangeSchema(t *testing.T) {
	p := parser{}
	u, err := url.Parse("athena://user:pass@athena.us-east-1.amazonaws.com/olddb?s3_staging_dir=s3://bucket/path")
	require.NoError(t, err)
	newURL := p.ChangeSchema(u, "newdb")
	require.Equal(t, "/newdb", newURL.Path)
}

func TestDriverVersion(t *testing.T) {
	d := &Driver{conn: &conn{}}
	require.Equal(t, "3", d.Version())
}

func TestDriverStmtBuilder(t *testing.T) {
	d := &Driver{}
	builder := d.StmtBuilder(migrate.PlanOptions{})
	require.NotNil(t, builder)
	require.Equal(t, byte('`'), builder.QuoteOpening)
	require.Equal(t, byte('`'), builder.QuoteClosing)
}
