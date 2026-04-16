// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package athena

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"ariga.io/atlas/schemahcl"
	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"
	"ariga.io/atlas/sql/sqlclient"
)

type (
	// Driver represents an AWS Athena driver for introspecting database schemas,
	// generating diff between schema elements and applying migrations changes.
	Driver struct {
		*conn
		schema.Differ
		schema.Inspector
		migrate.PlanApplier
	}

	// database connection and its information.
	conn struct {
		schema.ExecQuerier
		// The database/catalog this connection is bound to.
		database string
	}
)

var _ interface {
	migrate.StmtScanner
	schema.TypeParseFormatter
} = (*Driver)(nil)

// DriverName holds the name used for atlas URL-scheme registration (athena://).
const DriverName = "athena"

// sqlDriverName is the database/sql driver name registered by
// github.com/uber/athenadriver/go (see its constants.go: DriverName = "awsathena").
// It is distinct from DriverName, which is the Atlas URL scheme users type.
const sqlDriverName = "awsathena"

func init() {
	sqlclient.Register(
		DriverName,
		sqlclient.OpenerFunc(opener),
		sqlclient.RegisterDriverOpener(Open),
		sqlclient.RegisterCodec(codec, codec),
		sqlclient.RegisterURLParser(parser{}),
	)
}

func opener(_ context.Context, u *url.URL) (*sqlclient.Client, error) {
	database := strings.TrimPrefix(u.Path, "/")
	dsn, err := buildDSN(u, database)
	if err != nil {
		return nil, err
	}
	if err := applyAuthEnv(u); err != nil {
		return nil, err
	}
	db, err := sql.Open(sqlDriverName, dsn)
	if err != nil {
		return nil, err
	}
	drv, err := Open(db)
	if err != nil {
		if cerr := db.Close(); cerr != nil {
			err = fmt.Errorf("%w: %v", err, cerr)
		}
		return nil, err
	}
	drv.(*Driver).database = database
	return &sqlclient.Client{
		Name: DriverName,
		DB:   db,
		URL: &sqlclient.URL{
			URL:    u,
			DSN:    dsn,
			Schema: database,
		},
		Driver: drv,
	}, nil
}

// Open opens a new Athena driver.
func Open(db schema.ExecQuerier) (migrate.Driver, error) {
	c := &conn{ExecQuerier: db}
	return &Driver{
		conn:        c,
		Differ:      &sqlx.Diff{DiffDriver: &diff{conn: c}},
		Inspector:   &inspect{c},
		PlanApplier: &planApply{c},
	}, nil
}

// NormalizeRealm returns the normal representation of the given database.
func (d *Driver) NormalizeRealm(ctx context.Context, r *schema.Realm) (*schema.Realm, error) {
	return (&sqlx.DevDriver{Driver: d}).NormalizeRealm(ctx, r)
}

// NormalizeSchema returns the normal representation of the given database.
func (d *Driver) NormalizeSchema(ctx context.Context, s *schema.Schema) (*schema.Schema, error) {
	return (&sqlx.DevDriver{Driver: d}).NormalizeSchema(ctx, s)
}

// Lock implements the schema.Locker interface.
// Athena does not support advisory locks, so this is a no-op that returns immediately.
func (d *Driver) Lock(_ context.Context, _ string, _ time.Duration) (schema.UnlockFunc, error) {
	return func() error { return nil }, nil
}

// Snapshot implements migrate.Snapshoter.
func (d *Driver) Snapshot(ctx context.Context) (migrate.RestoreFunc, error) {
	s, err := d.InspectSchema(ctx, "", nil)
	if err != nil && !schema.IsNotExistError(err) {
		return nil, err
	}
	if s != nil {
		if len(s.Tables) > 0 {
			return nil, &migrate.NotCleanError{
				State:  schema.NewRealm(s),
				Reason: fmt.Sprintf("found table %q in schema %q", s.Tables[0].Name, s.Name),
			}
		}
		return d.SchemaRestoreFunc(s), nil
	}
	realm, err := d.InspectRealm(ctx, nil)
	if err != nil {
		return nil, err
	}
	if len(realm.Schemas) > 0 {
		return nil, &migrate.NotCleanError{State: realm, Reason: fmt.Sprintf("found schema %q", realm.Schemas[0].Name)}
	}
	return d.RealmRestoreFunc(realm), nil
}

// SchemaRestoreFunc returns a function that restores the given schema to its desired state.
func (d *Driver) SchemaRestoreFunc(desired *schema.Schema) migrate.RestoreFunc {
	return func(ctx context.Context) error {
		current, err := d.InspectSchema(ctx, desired.Name, nil)
		if err != nil {
			return err
		}
		changes, err := d.SchemaDiff(current, desired)
		if err != nil {
			return err
		}
		return d.ApplyChanges(ctx, changes)
	}
}

// RealmRestoreFunc returns a function that restores the given realm to its desired state.
func (d *Driver) RealmRestoreFunc(desired *schema.Realm) migrate.RestoreFunc {
	return func(ctx context.Context) error {
		current, err := d.InspectRealm(ctx, nil)
		if err != nil {
			return err
		}
		changes, err := d.RealmDiff(current, desired)
		if err != nil {
			return err
		}
		return d.ApplyChanges(ctx, changes)
	}
}

// CheckClean implements migrate.CleanChecker.
func (d *Driver) CheckClean(ctx context.Context, revT *migrate.TableIdent) error {
	if revT == nil {
		revT = &migrate.TableIdent{}
	}
	s, err := d.InspectSchema(ctx, "", nil)
	if err != nil && !schema.IsNotExistError(err) {
		return err
	}
	if s != nil {
		if len(s.Tables) == 0 || (revT.Schema == "" || s.Name == revT.Schema) && len(s.Tables) == 1 && s.Tables[0].Name == revT.Name {
			return nil
		}
		return &migrate.NotCleanError{
			State:  schema.NewRealm(s),
			Reason: fmt.Sprintf("found table %q in schema %q", s.Tables[0].Name, s.Name),
		}
	}
	r, err := d.InspectRealm(ctx, nil)
	if err != nil {
		return err
	}
	switch n := len(r.Schemas); {
	case n > 1:
		return &migrate.NotCleanError{State: r, Reason: fmt.Sprintf("found multiple schemas: %d", len(r.Schemas))}
	case n == 1 && r.Schemas[0].Name != revT.Schema:
		return &migrate.NotCleanError{State: r, Reason: fmt.Sprintf("found schema %q", r.Schemas[0].Name)}
	case n == 1 && len(r.Schemas[0].Tables) > 1:
		return &migrate.NotCleanError{State: r, Reason: fmt.Sprintf("found multiple tables: %d", len(r.Schemas[0].Tables))}
	case n == 1 && len(r.Schemas[0].Tables) == 1 && r.Schemas[0].Tables[0].Name != revT.Name:
		return &migrate.NotCleanError{State: r, Reason: fmt.Sprintf("found table %q", r.Schemas[0].Tables[0].Name)}
	}
	return nil
}

// Version returns the version of the connected database.
func (d *Driver) Version() string {
	return "3" // Athena engine version 3
}

// FormatType converts schema type to its column form in the database.
func (*Driver) FormatType(t schema.Type) (string, error) {
	return FormatType(t)
}

// ParseType returns the schema.Type value represented by the given string.
func (*Driver) ParseType(s string) (schema.Type, error) {
	return ParseType(s)
}

// StmtBuilder is a helper method used to build statements with Athena formatting.
func (*Driver) StmtBuilder(opts migrate.PlanOptions) *sqlx.Builder {
	return &sqlx.Builder{
		QuoteOpening: '`',
		QuoteClosing: '`',
		Schema:       opts.SchemaQualifier,
		Indent:       opts.Indent,
	}
}

// ScanStmts implements migrate.StmtScanner.
func (*Driver) ScanStmts(input string) ([]*migrate.Stmt, error) {
	return (&migrate.Scanner{
		ScannerOptions: migrate.ScannerOptions{
			MatchBegin:       false,
			MatchBeginAtomic: false,
			MatchDollarQuote: false,
		},
	}).Scan(input)
}

type parser struct{}

// ParseURL implements the sqlclient.URLParser interface.
//
// The Atlas URL is translated into the DSN consumed by uber/athenadriver, which
// is an s3:// URL carrying connection options as query parameters. All auth
// modes supported by boto3 can be selected via URL query parameters.
//
// Supported URL forms (region is extracted from the host or the region query param):
//
//  1. Static credentials (optionally with session_token):
//     athena://AKID:SECRET@athena.us-east-1.amazonaws.com/db
//     ?s3_staging_dir=s3://bucket/path[&session_token=TOKEN]
//
//  2. Default credential chain (env vars, ~/.aws/credentials, IAM role on EC2/ECS/EKS):
//     athena://athena.us-east-1.amazonaws.com/db?s3_staging_dir=s3://bucket/path
//
//  3. Named AWS profile (from ~/.aws/credentials and ~/.aws/config):
//     athena://athena.us-east-1.amazonaws.com/db
//     ?s3_staging_dir=s3://bucket/path&profile=myprofile
//
//  4. Assume role (sourcing credentials from the default chain, then STS AssumeRole):
//     athena://athena.us-east-1.amazonaws.com/db
//     ?s3_staging_dir=s3://bucket/path&role_arn=arn:aws:iam::123:role/atlas
//     [&role_session_name=atlas][&external_id=...]
//
//  5. Web identity (OIDC, used by EKS IRSA and GitHub Actions OIDC):
//     athena://athena.us-east-1.amazonaws.com/db
//     ?s3_staging_dir=s3://bucket/path
//     &role_arn=arn:aws:iam::123:role/atlas
//     &web_identity_token_file=/var/run/secrets/token
//
// For modes (2)-(5) the underlying aws-sdk-go default credential chain is used,
// so any additional mechanism it supports (SSO, IMDS, ECS task role, process
// credentials, etc.) works transparently.
func (parser) ParseURL(u *url.URL) *sqlclient.URL {
	database := strings.TrimPrefix(u.Path, "/")
	dsn, err := buildDSN(u, database)
	if err != nil {
		// Defer the error surfacing to sql.Open so that the URL parser
		// (which has no error return) keeps its contract.
		dsn = "s3://invalid?err=" + url.QueryEscape(err.Error())
	}
	return &sqlclient.URL{
		URL:    u,
		DSN:    dsn,
		Schema: database,
	}
}

// ChangeSchema implements the sqlclient.SchemaChanger interface.
func (parser) ChangeSchema(u *url.URL, s string) *url.URL {
	nu := *u
	nu.Path = "/" + s
	return &nu
}

// hostRegionRE matches `athena.<region>.amazonaws.com` and variants.
var hostRegionRE = regexp.MustCompile(`^athena[.-]([a-z]{2}-[a-z]+-\d+)\.amazonaws\.com(?:\.cn)?$`)

// buildDSN translates an atlas Athena URL into uber/athenadriver's native DSN.
//
// The resulting DSN has the form:
//
//	s3://<bucket>/<path>?region=<r>&db=<db>&accessID=<k>&secretAccessKey=<s>&sessionToken=<t>&AWSProfile=<p>...
//
// Credentials that require STS (role_arn / web_identity) are resolved by the
// underlying aws-sdk-go default credential chain; this function sets the
// appropriate environment variables (see applyAuthEnv) so they are picked up.
func buildDSN(u *url.URL, database string) (string, error) {
	q := u.Query()
	region := firstNonEmpty(q.Get("region"), q.Get("AWS_REGION"), regionFromHost(u.Host))
	if region == "" {
		return "", fmt.Errorf("athena: region is required; set the host to athena.<region>.amazonaws.com or pass ?region=<r>")
	}
	staging := firstNonEmpty(q.Get("s3_staging_dir"), q.Get("s3_output_location"), q.Get("output_bucket"))
	if !strings.HasPrefix(staging, "s3://") {
		return "", fmt.Errorf("athena: s3_staging_dir query parameter is required and must start with s3://")
	}
	out, err := url.Parse(staging)
	if err != nil {
		return "", fmt.Errorf("athena: invalid s3_staging_dir %q: %w", staging, err)
	}
	vals := url.Values{}
	vals.Set("region", region)
	if database != "" {
		vals.Set("db", database)
	}
	vals.Set("missingAsEmptyString", "true")
	vals.Set("WGRemoteCreation", "true")
	// Static credentials provided as userinfo - Mode 1.
	if u.User != nil {
		if akid := u.User.Username(); akid != "" {
			vals.Set("accessID", akid)
		}
		if secret, ok := u.User.Password(); ok && secret != "" {
			vals.Set("secretAccessKey", secret)
		}
	}
	// Session token (temporary STS credentials).
	if t := firstNonEmpty(q.Get("session_token"), q.Get("aws_session_token")); t != "" {
		vals.Set("sessionToken", t)
	}
	// Named profile - Mode 3.
	// Intentionally NOT setting AWSProfile on the athenadriver DSN: that path
	// uses credentials.NewSharedCredentials which reads ONLY ~/.aws/credentials
	// and does not support SSO / sso_session profiles defined in ~/.aws/config.
	// Instead we expose the profile via the AWS_PROFILE env var + AWS_SDK_LOAD_CONFIG=1
	// (see applyAuthEnv), which makes athenadriver fall through to
	// session.NewSession(&aws.Config{}), and aws-sdk-go's default session
	// honors shared config for SSO, role_arn source_profile, credential_process,
	// etc. The URL param is still parsed so it can be validated up-front.
	_ = firstNonEmpty(q.Get("profile"), q.Get("aws_profile"))
	// Optional workgroup (not an auth mode but useful alongside it).
	if wg := q.Get("workgroup"); wg != "" {
		vals.Set("workgroupName", wg)
	}
	out.RawQuery = vals.Encode()
	return out.String(), nil
}

// applyAuthEnv sets process-level AWS environment variables for auth modes
// that cannot be expressed purely via uber/athenadriver's DSN (assume role,
// web identity, profile-based shared config). The underlying aws-sdk-go
// default credential chain picks these up when the driver opens a session.
//
// Note: environment variables are process-global; this is a reasonable
// trade-off for CLI usage (the common case for Atlas) and avoids pulling
// aws-sdk-go-v2 into the core atlas module. Library users that need
// connection-scoped auth can set these variables themselves.
func applyAuthEnv(u *url.URL) error {
	q := u.Query()
	setIfUnset := func(key, val string) error {
		if val == "" {
			return nil
		}
		// Treat empty env vars as unset: users often clear an env var by
		// setting it to "" and expect a fresh value to take effect.
		if existing, ok := os.LookupEnv(key); ok && existing != "" {
			return nil
		}
		return os.Setenv(key, val)
	}
	// Honor profile by enabling shared-config mode so ~/.aws/config
	// (region, role_arn, sso_*, etc.) is read in addition to credentials.
	if p := firstNonEmpty(q.Get("profile"), q.Get("aws_profile")); p != "" {
		if err := setIfUnset("AWS_SDK_LOAD_CONFIG", "1"); err != nil {
			return err
		}
		if err := setIfUnset("AWS_PROFILE", p); err != nil {
			return err
		}
	}
	// Assume role - Mode 4.
	if arn := firstNonEmpty(q.Get("role_arn"), q.Get("aws_role_arn")); arn != "" {
		if err := setIfUnset("AWS_ROLE_ARN", arn); err != nil {
			return err
		}
		if name := firstNonEmpty(q.Get("role_session_name"), q.Get("aws_role_session_name")); name != "" {
			if err := setIfUnset("AWS_ROLE_SESSION_NAME", name); err != nil {
				return err
			}
		}
		// AWS SDK respects AWS_STS_REGIONAL_ENDPOINTS; external_id is not an
		// env var in SDK v1 - surface a clear error rather than silently
		// dropping it.
		if ext := q.Get("external_id"); ext != "" {
			return fmt.Errorf("athena: external_id is not supported via URL; set it in ~/.aws/config under the target profile")
		}
	}
	// Web identity / OIDC - Mode 5.
	if tf := firstNonEmpty(q.Get("web_identity_token_file"), q.Get("aws_web_identity_token_file")); tf != "" {
		if err := setIfUnset("AWS_WEB_IDENTITY_TOKEN_FILE", tf); err != nil {
			return err
		}
		// Web identity requires role_arn - validate early.
		if os.Getenv("AWS_ROLE_ARN") == "" {
			return fmt.Errorf("athena: web_identity_token_file requires role_arn")
		}
	}
	return nil
}

// regionFromHost extracts the AWS region from an Athena endpoint hostname.
// For example: "athena.us-east-1.amazonaws.com" -> "us-east-1".
// Returns an empty string if the host does not match the expected pattern.
func regionFromHost(host string) string {
	if m := hostRegionRE.FindStringSubmatch(host); len(m) == 2 {
		return m[1]
	}
	return ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

var specOptions []schemahcl.Option
