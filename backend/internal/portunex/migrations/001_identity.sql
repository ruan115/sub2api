-- Local synthetic recovery database only. The caller MUST run the entire file
-- in one explicit transaction against a fresh, isolated database.
-- This is not the original SQLx migration and is not a production upgrade.
CREATE SCHEMA portunex_identity;
CREATE EXTENSION citext WITH SCHEMA public VERSION '1.8';

CREATE TABLE portunex_identity.users (
    id bigint NOT NULL,
    email public.citext,
    password_phc text,
    points numeric(30,18) DEFAULT 0,
    role text DEFAULT 'user'::text,
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now(),
    deleted_at timestamp with time zone,
    can_purchase_subscription boolean NOT NULL DEFAULT false,
    daily_recharge_limit numeric(30,18) NOT NULL DEFAULT 0,
    CONSTRAINT users_pkey PRIMARY KEY (id),
    CONSTRAINT users_role_check CHECK ((role = ANY (ARRAY['admin'::text, 'user'::text])))
);

CREATE TABLE portunex_identity.auth_sessions (
    id bigint NOT NULL,
    user_id bigint NOT NULL,
    token text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    ip_address text,
    user_agent text,
    created_at timestamp with time zone DEFAULT now(),
    last_used_at timestamp with time zone DEFAULT now(),
    deleted_at timestamp with time zone,
    CONSTRAINT auth_sessions_pkey PRIMARY KEY (id),
    CONSTRAINT auth_sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES portunex_identity.users(id) ON DELETE CASCADE
);

CREATE TABLE portunex_identity.api_keys (
    id bigint NOT NULL,
    user_id bigint,
    key_text text,
    prefix text,
    active boolean DEFAULT true,
    settings jsonb,
    created_at timestamp with time zone DEFAULT now(),
    rotated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    name text,
    last_used_at timestamp with time zone,
    CONSTRAINT api_keys_pkey PRIMARY KEY (id),
    CONSTRAINT api_keys_user_id_fkey FOREIGN KEY (user_id) REFERENCES portunex_identity.users(id) ON DELETE CASCADE
);

CREATE INDEX idx_api_keys_created_at_desc ON portunex_identity.api_keys USING btree (created_at DESC) WHERE (deleted_at IS NULL);
CREATE INDEX idx_api_keys_deleted_at ON portunex_identity.api_keys USING btree (deleted_at) WHERE (deleted_at IS NULL);
CREATE UNIQUE INDEX idx_api_keys_key_text_unique ON portunex_identity.api_keys USING btree (key_text) WHERE (deleted_at IS NULL);
CREATE INDEX idx_api_keys_last_used ON portunex_identity.api_keys USING btree (last_used_at DESC NULLS LAST) WHERE (deleted_at IS NULL);
CREATE INDEX idx_api_keys_prefix_text ON portunex_identity.api_keys USING btree (prefix, key_text) WHERE (deleted_at IS NULL);
CREATE INDEX idx_api_keys_settings ON portunex_identity.api_keys USING gin (settings);
CREATE INDEX idx_api_keys_user_id_id ON portunex_identity.api_keys USING btree (user_id, id DESC) WHERE (deleted_at IS NULL);
CREATE INDEX idx_auth_sessions_deleted_at ON portunex_identity.auth_sessions USING btree (deleted_at) WHERE (deleted_at IS NULL);
CREATE INDEX idx_auth_sessions_expires ON portunex_identity.auth_sessions USING btree (expires_at);
CREATE INDEX idx_auth_sessions_token ON portunex_identity.auth_sessions USING btree (token);
CREATE UNIQUE INDEX idx_auth_sessions_token_unique ON portunex_identity.auth_sessions USING btree (token) WHERE (deleted_at IS NULL);
CREATE INDEX idx_auth_sessions_user ON portunex_identity.auth_sessions USING btree (user_id);
CREATE INDEX idx_users_deleted_at ON portunex_identity.users USING btree (deleted_at) WHERE (deleted_at IS NULL);
CREATE UNIQUE INDEX idx_users_email_unique ON portunex_identity.users USING btree (email) WHERE (deleted_at IS NULL);
