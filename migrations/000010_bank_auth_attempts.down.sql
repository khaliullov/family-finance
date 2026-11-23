-- Rollback Migration 000010: Bank Authentication Attempts
DROP TABLE IF EXISTS public.bank_auth_attempts CASCADE;
