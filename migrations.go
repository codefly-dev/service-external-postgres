package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (s *Runtime) migrationPath(ctx context.Context) (string, error) {
	absolutePath := s.Local("migrations")
	exists, err := shared.DirectoryExists(ctx, absolutePath)
	if err != nil {
		return "", s.Wool.Wrapf(err, "can check migration directory")
	}

	if !exists {
		s.Wool.Debug("no migration folder found", wool.DirField(absolutePath))
		return "", nil
	}
	u := url.URL{
		Scheme: "file",
		Path:   absolutePath,
	}
	return u.String(), nil
}

func (s *Runtime) applyMigration(ctx context.Context) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	// Check if we have migrations to apply
	migrationPath, err := s.migrationPath(ctx)
	if err != nil {
		return s.Wool.Wrapf(err, "can check migration directory")
	}
	if migrationPath == "" {
		return nil
	}

	s.Wool.Debug("migrations", wool.Field("connection", s.connection))
	maxRetry := 3
	for retry := 0; retry < maxRetry; retry++ {
		db, err := sql.Open("postgres", s.connection)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot open database")
		}
		driver, err := postgres.WithInstance(db, &postgres.Config{DatabaseName: s.Settings.DatabaseName})
		if err != nil {
			time.Sleep(time.Second)
			continue
		}

		m, err := migrate.NewWithDatabaseInstance(
			migrationPath,
			s.Settings.DatabaseName, driver)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create migration")
		}
		if err := m.Up(); err == nil {
			return nil
		} else {
			if errors.Is(err, migrate.ErrNoChange) {
				return nil
			}
			return s.Wool.Wrapf(err, "can't apply migration")
		}
	}
	return s.Wool.NewError("cannot apply migration: retries exceeded")
}

func (s *Runtime) updateMigration(ctx context.Context, migrationFile string) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	// Extract the migration number
	base := filepath.Base(migrationFile)
	s.Wool.Info(fmt.Sprintf("applying migration: %v", base))
	_migrationNumber := strings.Split(base, "_")[0]
	migrationNumber, err := strconv.Atoi(_migrationNumber)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot parse migration number")
	}

	db, err := sql.Open("postgres", s.connection)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot open database")
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{DatabaseName: s.Settings.DatabaseName})
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create driver")
	}

	migrationPath, err := s.migrationPath(ctx)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot get migration path")
	}
	if migrationPath == "" {
		return nil
	}

	m, err := migrate.NewWithDatabaseInstance(
		migrationPath,
		s.Settings.DatabaseName, driver)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create migration")
	}

	if err := m.Force(migrationNumber); err != nil {
		return s.Wool.Wrapf(err, "cannot force migration")
	}
	// Now, re-apply migration by moving down.
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return s.Wool.Wrapf(err, "cannot apply migration")
	}
	// Now, re-apply migration by moving up.
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return s.Wool.Wrapf(err, "cannot apply migration")
	}
	// Optionally, check if there are any errors in the migration process
	var errMigrate migrate.ErrDirty
	if errors.As(err, &errMigrate) {
		return s.Wool.Wrapf(err, "migration is dirty")
	}
	return s.Wool.Wrapf(err, "migration applied")
}
