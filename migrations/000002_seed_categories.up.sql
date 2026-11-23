-- =============================================================================
-- Migration 000002: Seed Default Categories
-- Seeds standard category hierarchy for any existing families.
-- =============================================================================

DO $$
DECLARE
    f RECORD;
BEGIN
    FOR f IN SELECT id FROM public.families LOOP
        INSERT INTO public.categories (family_id, name, type, icon, color, is_system) VALUES
            (f.id, 'Groceries', 'expense', 'shopping-cart', '#2ECC71', TRUE),
            (f.id, 'Housing & Utilities', 'expense', 'home', '#3498DB', TRUE),
            (f.id, 'Dining Out', 'expense', 'utensils', '#E67E22', TRUE),
            (f.id, 'Transportation', 'expense', 'car', '#9B59B6', TRUE),
            (f.id, 'Healthcare', 'expense', 'heart-pulse', '#E74C3C', TRUE),
            (f.id, 'Entertainment & Leisure', 'expense', 'film', '#F1C40F', TRUE),
            (f.id, 'Shopping & Personal', 'expense', 'shopping-bag', '#1ABC9C', TRUE),
            (f.id, 'Education', 'expense', 'graduation-cap', '#34495E', TRUE),
            (f.id, 'Family & Children', 'expense', 'users', '#E84393', TRUE),
            (f.id, 'Financial & Charges', 'expense', 'landmark', '#6C5CE7', TRUE),
            (f.id, 'Income / Salary', 'income', 'wallet', '#27AE60', TRUE),
            (f.id, 'Transfers & Repayments', 'transfer', 'repeat', '#7F8C8D', TRUE)
        ON CONFLICT (family_id, parent_id, name) DO NOTHING;
    END LOOP;
END $$;
