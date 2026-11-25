-- Cluster provisioning of the catalog database roles (ADR-0003 section 6). Run once as a superuser per cluster,
-- before `catalog migrate`; deployment owns it (AM12: not run anywhere yet).
--
--   psql -v migrator_password=... -v app_password=... -v relay_password=... -f provision-roles.sql
--
-- Passwords come from the secret store by reference; never put a value in this file.
--
--   catalog_migrator  owns the database and its tables; runs migrations only (never at serve time)
--   catalog_app       DML only, NOSUPERUSER NOBYPASSRLS, not an owner (the service's runtime role)
--   catalog_relay     the outbox table only (relay to the bus)

SELECT format('CREATE ROLE catalog_migrator LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD %L', :'migrator_password')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'catalog_migrator') \gexec
SELECT format('CREATE ROLE catalog_app LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD %L', :'app_password')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'catalog_app') \gexec
SELECT format('CREATE ROLE catalog_relay LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD %L', :'relay_password')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'catalog_relay') \gexec

SELECT 'CREATE DATABASE catalog OWNER catalog_migrator'
 WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'catalog') \gexec
