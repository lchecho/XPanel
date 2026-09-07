package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func Migrate(ctx context.Context, db *sql.DB) error {
	provider, err := migrationProvider(db)
	if err != nil {
		return err
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

func migrationProvider(db *sql.DB) (*goose.Provider, error) {
	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("open embedded migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, files)
	if err != nil {
		return nil, fmt.Errorf("create migration provider: %w", err)
	}
	return provider, nil
}

// migrateUpTo 迁移到指定版本，仅供迁移测试构造「旧库升级」场景。
func migrateUpTo(ctx context.Context, db *sql.DB, version int64) error {
	provider, err := migrationProvider(db)
	if err != nil {
		return err
	}
	if _, err := provider.UpTo(ctx, version); err != nil {
		return fmt.Errorf("apply migrations up to %d: %w", version, err)
	}
	return nil
}

// migrateDownTo 回滚到指定版本，仅供迁移测试验证 -- +goose Down 的可执行性。
func migrateDownTo(ctx context.Context, db *sql.DB, version int64) error {
	provider, err := migrationProvider(db)
	if err != nil {
		return err
	}
	if _, err := provider.DownTo(ctx, version); err != nil {
		return fmt.Errorf("roll back migrations: %w", err)
	}
	return nil
}
