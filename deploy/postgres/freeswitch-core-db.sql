-- Prepares a PostgreSQL database for FreeSWITCH's core db (call tracking, docs/call-tracking.md). Run it once in
-- that database as a user allowed to create event triggers (a superuser; on Aurora or RDS, the master user), before
-- or after FreeSWITCH first starts; running it again changes nothing.
--
--   psql "host=... dbname=freeswitch user=..." -v ON_ERROR_STOP=1 -f deploy/postgres/freeswitch-core-db.sql
--
-- Why: FreeSWITCH creates its tables with length-limited columns (varchar(1024) for a caller's name, say), which
-- SQLite never enforces and PostgreSQL does. A call whose caller name is longer gets its write refused, and
-- FreeSWITCH writes the core db in batches, one transaction each: the refused statement ends the batch, and its
-- COMMIT rolls back every statement before it, which FreeSWITCH never retries. Those are other calls' writes: the
-- deletes of ended calls (rows left for calls long gone) and the call tracking another node needs to take over a
-- lost node's calls. So every length-limited column becomes text, which PostgreSQL never refuses for length.
--
-- What it does: freeswitch_unbound_columns() turns every length-limited varchar column of this schema's tables into
-- text, dropping and recreating the views over them (FreeSWITCH's basic_calls and detailed_calls, from their own
-- definitions) around the change, which PostgreSQL refuses while a view depends on a column. It runs now, for the
-- tables FreeSWITCH has made already, and from an event trigger whenever a table is created or altered, for the
-- tables a first start or an upgrade makes later. FreeSWITCH keeps the tables it finds (it checks them with a SELECT
-- of the columns it needs), so the change lasts. Meant for a database FreeSWITCH alone uses: it changes every table
-- in the schema.
--
-- Without the right to create event triggers, leave out the event trigger and run SELECT freeswitch_unbound_columns()
-- after FreeSWITCH first starts and after each FreeSWITCH upgrade.

CREATE OR REPLACE FUNCTION freeswitch_unbound_columns() RETURNS integer
LANGUAGE plpgsql AS $fn$
DECLARE
  names text[] := '{}';
  defs text[] := '{}';
  r record;
  changed integer := 0;
BEGIN
  -- The ALTER TABLEs below fire the event trigger again.
  IF current_setting('freeswitch.unbounding', true) = 'on' THEN
    RETURN 0;
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns c
    JOIN information_schema.tables t USING (table_schema, table_name)
    WHERE c.table_schema = current_schema() AND t.table_type = 'BASE TABLE'
      AND c.data_type = 'character varying' AND c.character_maximum_length IS NOT NULL
  ) THEN
    RETURN 0;
  END IF;
  PERFORM set_config('freeswitch.unbounding', 'on', true);

  FOR r IN SELECT oid, relname FROM pg_class
    WHERE relkind = 'v' AND relnamespace = current_schema()::regnamespace ORDER BY oid
  LOOP
    names := names || r.relname::text;
    defs := defs || pg_get_viewdef(r.oid);
  END LOOP;
  FOR i IN REVERSE coalesce(array_length(names, 1), 0)..1 LOOP
    EXECUTE format('DROP VIEW %I', names[i]);
  END LOOP;

  FOR r IN SELECT c.table_name, c.column_name FROM information_schema.columns c
    JOIN information_schema.tables t USING (table_schema, table_name)
    WHERE c.table_schema = current_schema() AND t.table_type = 'BASE TABLE'
      AND c.data_type = 'character varying' AND c.character_maximum_length IS NOT NULL
    ORDER BY c.table_name, c.ordinal_position
  LOOP
    EXECUTE format('ALTER TABLE %I ALTER COLUMN %I TYPE text', r.table_name, r.column_name);
    changed := changed + 1;
  END LOOP;

  FOR i IN 1..coalesce(array_length(names, 1), 0) LOOP
    EXECUTE format('CREATE VIEW %I AS %s', names[i], defs[i]);
  END LOOP;
  PERFORM set_config('freeswitch.unbounding', 'off', true);
  RETURN changed;
END
$fn$;

CREATE OR REPLACE FUNCTION freeswitch_unbound_columns_on_ddl() RETURNS event_trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM freeswitch_unbound_columns();
END
$fn$;

DROP EVENT TRIGGER IF EXISTS freeswitch_unbound_columns;
CREATE EVENT TRIGGER freeswitch_unbound_columns ON ddl_command_end
  WHEN TAG IN ('CREATE TABLE', 'ALTER TABLE')
  EXECUTE FUNCTION freeswitch_unbound_columns_on_ddl();

SELECT freeswitch_unbound_columns() AS columns_unbounded;
