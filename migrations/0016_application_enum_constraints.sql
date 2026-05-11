-- 0016_application_enum_constraints.sql
-- Defense-in-depth: enforce stage / source enums at the DB layer too.
-- The handler already rejects bad values via isValidStage / isValidSource
-- but a CHECK constraint catches anything that arrives outside the
-- handler path (direct psql edits, future ingestion paths, mistakes).
--
-- NOT VALID + a separate VALIDATE step keeps the prod lock-window short
-- on tables that may have grown: the ADD takes only a short ACCESS
-- EXCLUSIVE, while VALIDATE runs concurrently against existing rows
-- with a weaker share-update lock. Re-running this migration is a
-- no-op (the DROP guards against the prior partial state).
--
-- Idempotent: safe to re-run.

-- Stage: 7-value enum.
ALTER TABLE job_tracker.applications
  DROP CONSTRAINT IF EXISTS applications_stage_chk;

ALTER TABLE job_tracker.applications
  ADD CONSTRAINT applications_stage_chk
  CHECK (stage IN ('INTERESTED','APPLIED','PHONE_SCREEN','TECHNICAL','ONSITE','OFFER','REJECTED'))
  NOT VALID;

ALTER TABLE job_tracker.applications
  VALIDATE CONSTRAINT applications_stage_chk;

-- Source: 5-value enum + NULL/empty allowed.
-- Stored as TEXT today; existing rows may carry free-form values
-- because `source` was never server-validated until this migration's
-- companion code change. Normalise existing rows first so the
-- VALIDATE step doesn't bomb:
--   1. Common case-variants (linkedin → LINKEDIN, etc.) get canonicalised
--   2. Anything else non-empty gets bucketed to 'OTHER' (the spec
--      explicitly drops 'Indeed' and other miscellaneous sources)
--
-- This is destructive for unknown values — they all become 'OTHER'.
-- Acceptable here because (a) source was free-form before so no
-- analytics depended on the original strings, and (b) the spec's
-- 5-value enum is the contract going forward. If you have audit
-- requirements, run a SELECT first to inventory pre-normalisation
-- values: `SELECT DISTINCT source FROM job_tracker.applications;`.
UPDATE job_tracker.applications
SET source = CASE upper(trim(source))
  WHEN 'LINKEDIN'      THEN 'LINKEDIN'
  WHEN 'NAUKRI'        THEN 'NAUKRI'
  WHEN 'REFERRAL'      THEN 'REFERRAL'
  WHEN 'COMPANY_SITE'  THEN 'COMPANY_SITE'
  WHEN 'COMPANY-SITE'  THEN 'COMPANY_SITE'
  WHEN 'COMPANYSITE'   THEN 'COMPANY_SITE'
  WHEN 'OTHER'         THEN 'OTHER'
  ELSE 'OTHER'
END
WHERE source IS NOT NULL
  AND source <> ''
  AND source NOT IN ('LINKEDIN','NAUKRI','REFERRAL','COMPANY_SITE','OTHER');

ALTER TABLE job_tracker.applications
  DROP CONSTRAINT IF EXISTS applications_source_chk;

ALTER TABLE job_tracker.applications
  ADD CONSTRAINT applications_source_chk
  CHECK (source IS NULL OR source = '' OR source IN ('LINKEDIN','NAUKRI','REFERRAL','COMPANY_SITE','OTHER'))
  NOT VALID;

ALTER TABLE job_tracker.applications
  VALIDATE CONSTRAINT applications_source_chk;
