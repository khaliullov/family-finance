package database

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DefaultCategoryDefinition defines a standard system category.
type DefaultCategoryDefinition struct {
	Name  string
	Type  string
	Icon  string
	Color string
}

// DefaultSystemCategories lists the 12 canonical system categories seeded for every family.
var DefaultSystemCategories = []DefaultCategoryDefinition{
	{Name: "Groceries", Type: "expense", Icon: "shopping-cart", Color: "#2ECC71"},
	{Name: "Housing & Utilities", Type: "expense", Icon: "home", Color: "#3498DB"},
	{Name: "Dining Out", Type: "expense", Icon: "utensils", Color: "#E67E22"},
	{Name: "Transportation", Type: "expense", Icon: "car", Color: "#9B59B6"},
	{Name: "Healthcare", Type: "expense", Icon: "heart-pulse", Color: "#E74C3C"},
	{Name: "Entertainment & Leisure", Type: "expense", Icon: "film", Color: "#F1C40F"},
	{Name: "Shopping & Personal", Type: "expense", Icon: "shopping-bag", Color: "#1ABC9C"},
	{Name: "Education", Type: "expense", Icon: "graduation-cap", Color: "#34495E"},
	{Name: "Family & Children", Type: "expense", Icon: "users", Color: "#E84393"},
	{Name: "Financial & Charges", Type: "expense", Icon: "landmark", Color: "#6C5CE7"},
	{Name: "Income / Salary", Type: "income", Icon: "wallet", Color: "#27AE60"},
	{Name: "Transfers & Repayments", Type: "transfer", Icon: "repeat", Color: "#7F8C8D"},
}

// SeedDefaultCategories inserts standard system categories for a given family.
// Uses ON CONFLICT (family_id, parent_id, name) DO NOTHING alongside PostgreSQL 16+
// UNIQUE NULLS NOT DISTINCT constraint to guarantee:
// 1. Existing and newly registered families receive identical standard category trees.
// 2. Re-running registration or seeding is strictly idempotent (zero duplicates).
// 3. User-created categories (is_system = FALSE) are never overwritten or touched.
func SeedDefaultCategories(ctx context.Context, tx pgx.Tx, familyID uuid.UUID) error {
	query := `
		INSERT INTO public.categories (family_id, parent_id, name, type, icon, color, is_system)
		VALUES ($1, NULL, $2, $3, $4, $5, TRUE)
		ON CONFLICT (family_id, parent_id, name) DO NOTHING
	`
	for _, cat := range DefaultSystemCategories {
		_, err := tx.Exec(ctx, query, familyID, cat.Name, cat.Type, cat.Icon, cat.Color)
		if err != nil {
			return fmt.Errorf("failed to seed default category %q: %w", cat.Name, err)
		}
	}
	return nil
}
