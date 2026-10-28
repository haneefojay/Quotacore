#!/bin/sh
# Runs once, when the postgres image initialises an empty data directory (IP-15 D3,
# deployment.md section 7). It creates the role the service connects as: not a superuser, unable
# to create databases or roles, and the owner of its own database, which in PostgreSQL 16 makes it
# the owner of that database's public schema too.
set -eu
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
  --set=pw="$QUOTACORE_POSTGRES_PASSWORD" <<'SQL'
CREATE ROLE quotacore LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD :'pw';
CREATE DATABASE quotacore OWNER quotacore;
REVOKE ALL ON DATABASE quotacore FROM PUBLIC;
SQL
