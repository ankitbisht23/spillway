-- The audit log (CP-13, SEC-08, ADR-0036, ADR-0040): one record per
-- successful mutation, written in the mutation's transaction. Append-only,
-- enforced twice: the app role may only SELECT and INSERT, and triggers
-- refuse UPDATE, DELETE and TRUNCATE from every role, the owner included,
-- unless a reviewed migration drops them. Release that may contract
-- anything added here: none needed, this release only adds.

-- +goose Up

CREATE TABLE audit_log (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at               timestamptz NOT NULL DEFAULT clock_timestamp(),
    actor_sub        text NOT NULL,
    actor_name       text NOT NULL,
    actor_roles      text[] NOT NULL,
    action           text NOT NULL,
    entity           text NOT NULL,
    entity_id        text,
    diff             jsonb NOT NULL,
    note             text,
    request_id       text NOT NULL,
    config_version   bigint,
    identity_version bigint
);
CREATE INDEX audit_log_entity ON audit_log (entity, entity_id, id);
CREATE INDEX audit_log_actor ON audit_log (actor_sub, id);
CREATE INDEX audit_log_at ON audit_log (at);

-- The default privileges of 00001 granted the app role UPDATE and DELETE;
-- take them back, and TRUNCATE for good measure.
-- contract: the REVOKE narrows only audit_log, created above in this file,
-- which no earlier release's binary uses.
REVOKE ALL ON audit_log FROM spillway_app;
GRANT SELECT, INSERT ON audit_log TO spillway_app;

-- +goose StatementBegin
CREATE FUNCTION audit_log_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_log_no_update_or_delete BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();
