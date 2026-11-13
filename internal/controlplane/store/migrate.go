package store

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrations embed.FS

// SchemaVersion is the newest migration this binary carries: the schema
// version `controlplane serve` needs (CP-19).
func SchemaVersion() int64 {
	names, _ := fs.Glob(migrations, "migrations/*.sql")
	var v int64
	for _, name := range names {
		n, _ := strconv.ParseInt(strings.SplitN(path.Base(name), "_", 2)[0], 10, 64)
		v = max(v, n)
	}
	return v
}

// Migrate applies the pending migrations to the database at dsn as its
// owner, under Postgres's session advisory lock so that two runs never
// interleave, then lets the app role log in with appPassword (CP-19,
// ADR-0036, ADR-0040). It returns the versions it applied, none when the
// schema is current.
func Migrate(ctx context.Context, dsn, ownerPassword, appPassword string) ([]int64, error) {
	if appPassword == "" {
		return nil, errors.New("the app role's password is empty")
	}
	cfg, err := poolConfig(dsn, ownerPassword)
	if err != nil {
		return nil, err
	}
	sqlDB := stdlib.OpenDB(*cfg.ConnConfig)
	defer func() { _ = sqlDB.Close() }()
	applied, err := up(ctx, sqlDB)
	if err != nil {
		return nil, err
	}
	verifier, err := scramVerifier(appPassword)
	if err != nil {
		return nil, err
	}
	// DDL takes no parameters, so the verifier, base64 and digits only, is
	// a literal. It, not the password, reaches the server (ADR-0040).
	stmt := "ALTER ROLE spillway_app WITH LOGIN PASSWORD '" + verifier + "'" //nolint:gosec // G202: base64 and digits we computed
	if _, err := sqlDB.ExecContext(ctx, stmt); err != nil {
		return nil, fmt.Errorf("app role: %w", err)
	}
	return applied, nil
}

func up(ctx context.Context, sqlDB *sql.DB) ([]int64, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, err
	}
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, err
	}
	results, err := p.Up(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	var applied []int64
	for _, r := range results {
		applied = append(applied, r.Source.Version)
	}
	return applied, nil
}

// scramVerifier returns password's SCRAM-SHA-256 verifier as Postgres stores
// it (RFC 7677, 4096 iterations, a 16-byte random salt), so the plaintext
// never reaches the server or its statement log (ADR-0040). Passwords are
// used as given, without SASLprep, which matters only beyond ASCII.
func scramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	const iterations = 4096
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	mac := func(msg string) []byte {
		h := hmac.New(sha256.New, salted)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	stored := sha256.Sum256(mac("Client Key"))
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(stored[:]), b64(mac("Server Key"))), nil
}
