-- =============================================================================
-- Migration 000002: Seed Categories - Down Migration
-- Removes seeded system categories from all families.
-- =============================================================================

DELETE FROM public.categories WHERE is_system = TRUE;
