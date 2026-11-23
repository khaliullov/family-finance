package database

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCategoryInitialization_ExistingAndNewFamilies(t *testing.T) {
	ctx := context.Background()
	dbURL := getTestDatabaseURL()

	migrationsDir, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatalf("failed to get migrations directory: %v", err)
	}

	// 1. Reset database to migration version 1 (fresh schema before seed migration)
	m, err := migrate.New("file://"+migrationsDir, dbURL)
	if err != nil {
		t.Fatalf("migrate.New failed: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(1); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to version 1 failed: %v", err)
	}

	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to test db: %v", err)
	}
	defer pool.Close()

	// 2. Insert an "existing" family prior to running migration 000002
	existingFamID := uuid.New()
	existingUserID := uuid.New()
	err = WithFamilyUserTx(ctx, pool, existingFamID, existingUserID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO public.users (id, email, password_hash, first_name) VALUES ($1, $2, 'hash', 'Existing')",
			existingUserID, existingFamID.String()+"@existing.test")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO public.families (id, name) VALUES ($1, 'Existing Family')", existingFamID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO public.family_members (id, family_id, user_id, role) VALUES ($1, $2, $3, 'owner')",
			uuid.New(), existingFamID, existingUserID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to insert existing family: %v", err)
	}

	// Also insert a user-created custom category for the existing family
	err = WithFamilyTx(ctx, pool, existingFamID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.categories (family_id, parent_id, name, type, icon, color, is_system)
			VALUES ($1, NULL, 'My Custom Hobbies', 'expense', 'palette', '#123456', FALSE)
		`, existingFamID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to insert user custom category: %v", err)
	}

	// 3. Now run migration 000002_seed_categories (up to version 2)
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate.Up failed: %v", err)
	}

	// Verify existing family received 12 system categories + 1 custom category
	var sysCatCount int
	err = WithFamilyTx(ctx, pool, existingFamID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM public.categories WHERE family_id = $1 AND is_system = TRUE", existingFamID).Scan(&sysCatCount)
	})
	if err != nil || sysCatCount != 12 {
		t.Fatalf("expected 12 system categories for existing family, got count=%d, err=%v", sysCatCount, err)
	}

	var customCatCount int
	err = WithFamilyTx(ctx, pool, existingFamID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM public.categories WHERE family_id = $1 AND is_system = FALSE AND name = 'My Custom Hobbies'", existingFamID).Scan(&customCatCount)
	})
	if err != nil || customCatCount != 1 {
		t.Fatalf("expected user custom category to remain intact, got count=%d, err=%v", customCatCount, err)
	}

	// 4. Test Newly Registered Family initialization via SeedDefaultCategories
	newFamID := uuid.New()
	newUserID := uuid.New()
	err = WithFamilyUserTx(ctx, pool, newFamID, newUserID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO public.users (id, email, password_hash, first_name) VALUES ($1, $2, 'hash', 'New')",
			newUserID, newFamID.String()+"@new.test")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO public.families (id, name) VALUES ($1, 'Newly Registered Family')", newFamID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO public.family_members (id, family_id, user_id, role) VALUES ($1, $2, $3, 'owner')",
			uuid.New(), newFamID, newUserID)
		if err != nil {
			return err
		}

		// Seed standard system categories during registration bootstrap
		return SeedDefaultCategories(ctx, tx, newFamID)
	})
	if err != nil {
		t.Fatalf("failed to register new family with seeded categories: %v", err)
	}

	// Verify newly registered family received the exact same 12 default categories
	var newSysCount int
	err = WithFamilyTx(ctx, pool, newFamID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM public.categories WHERE family_id = $1 AND is_system = TRUE", newFamID).Scan(&newSysCount)
	})
	if err != nil || newSysCount != 12 {
		t.Fatalf("expected 12 system categories for new family, got count=%d, err=%v", newSysCount, err)
	}

	// 5. Test Idempotency: Re-running registration / seeding does NOT create duplicates
	err = WithFamilyTx(ctx, pool, newFamID, func(tx pgx.Tx) error {
		return SeedDefaultCategories(ctx, tx, newFamID)
	})
	if err != nil {
		t.Fatalf("re-running SeedDefaultCategories failed: %v", err)
	}

	var afterReRunCount int
	err = WithFamilyTx(ctx, pool, newFamID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM public.categories WHERE family_id = $1 AND is_system = TRUE", newFamID).Scan(&afterReRunCount)
	})
	if err != nil || afterReRunCount != 12 {
		t.Fatalf("expected still exactly 12 system categories after re-seeding, got count=%d, err=%v", afterReRunCount, err)
	}

	// 6. Test User-Created Category Conflict Protection:
	// If a user created a category with name "Groceries" as custom (is_system = FALSE),
	// seeding does NOT overwrite its custom flag or metadata.
	conflictFamID := uuid.New()
	conflictUserID := uuid.New()
	err = WithFamilyUserTx(ctx, pool, conflictFamID, conflictUserID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO public.users (id, email, password_hash, first_name) VALUES ($1, $2, 'hash', 'Conflict')",
			conflictUserID, conflictFamID.String()+"@conflict.test")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO public.families (id, name) VALUES ($1, 'Conflict Family')", conflictFamID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO public.family_members (id, family_id, user_id, role) VALUES ($1, $2, $3, 'owner')",
			uuid.New(), conflictFamID, conflictUserID)
		if err != nil {
			return err
		}
		// Pre-create user custom category "Groceries"
		_, err = tx.Exec(ctx, `
			INSERT INTO public.categories (family_id, parent_id, name, type, icon, color, is_system)
			VALUES ($1, NULL, 'Groceries', 'expense', 'custom-icon', '#999999', FALSE)
		`, conflictFamID)
		if err != nil {
			return err
		}

		// Now attempt to seed system categories
		return SeedDefaultCategories(ctx, tx, conflictFamID)
	})
	if err != nil {
		t.Fatalf("failed during conflict family setup: %v", err)
	}

	// Assert user's "Groceries" is still is_system = FALSE with custom-icon and #999999
	var isSystem bool
	var icon, color string
	err = WithFamilyTx(ctx, pool, conflictFamID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT is_system, icon, color FROM public.categories
			WHERE family_id = $1 AND name = 'Groceries'
		`, conflictFamID).Scan(&isSystem, &icon, &color)
	})
	if err != nil {
		t.Fatalf("failed to query Groceries category: %v", err)
	}
	if isSystem != false || icon != "custom-icon" || color != "#999999" {
		t.Fatalf("expected user-created category to remain untouched (is_system=false, custom-icon, #999999), got is_system=%v icon=%s color=%s",
			isSystem, icon, color)
	}
}
