-- Copied from CPAMP (Copyright (c) 2026 Seakee), MIT; see CPAMP-LICENSE.
create table if not exists account_quota_cycles (
			id integer primary key autoincrement,
			activation_id integer not null,
			provider_cycle_key text not null,
			state text not null,
			scheduled_start_ms integer,
			scheduled_end_ms integer,
			actual_start_ms integer not null,
			actual_end_ms integer,
			duration_seconds integer,
			boundary_accuracy text not null,
			end_reason text,
			first_observation_id integer,
			last_observation_id integer,
			parent_cycle_id integer,
			created_at_ms integer not null,
			updated_at_ms integer not null,
			unique(activation_id, provider_cycle_key),
			foreign key(activation_id) references account_quota_window_activations(id),
			foreign key(first_observation_id) references account_quota_observations(id),
			foreign key(last_observation_id) references account_quota_observations(id),
			foreign key(parent_cycle_id) references account_quota_cycles(id)
		);

create table if not exists account_quota_observations (
			id integer primary key autoincrement,
			observation_hash text not null unique,
			account_key text not null,
			provider text not null,
			source text not null,
			source_observation_id text,
			inventory_scope_key text not null,
			inventory_mode text not null,
			observed_at_ms integer not null,
			window_count integer not null default 0,
			lifecycle_applied integer not null default 1,
			created_at_ms integer not null
		);

create table if not exists account_quota_snapshots (
			id integer primary key autoincrement,
			observation_id integer,
			logical_window_id integer,
			activation_id integer,
			cycle_id integer,
			account_key text not null,
			provider text not null,
			provider_window_id text not null,
			window_kind text not null,
			window_mode text not null,
			model_scope_kind text not null,
			model_scope_key text,
			model_ids_json text,
			scope_fingerprint text not null default '',
			content_hash text not null default '',
			source text not null,
			source_observation_id text,
			observed_at_ms integer not null,
			boundary_accuracy text not null,
			cycle_start_ms integer,
			cycle_end_ms integer,
			duration_seconds integer,
			used_percent real,
			remaining_percent real,
			used_value real,
			limit_value real,
			quota_unit text,
			reset_credits_available integer,
			reset_credits_json text,
			plan_type text,
			created_at_ms integer not null
		);

create table if not exists account_quota_window_activations (
			id integer primary key autoincrement,
			window_id integer not null,
			generation integer not null,
			status text not null,
			activated_at_ms integer not null,
			deactivated_at_ms integer,
			activation_accuracy text not null,
			deactivation_reason text,
			activate_observation_id integer,
			deactivate_observation_id integer,
			created_at_ms integer not null,
			updated_at_ms integer not null,
			unique(window_id, generation),
			foreign key(window_id) references account_quota_windows(id),
			foreign key(activate_observation_id) references account_quota_observations(id),
			foreign key(deactivate_observation_id) references account_quota_observations(id)
		);

create table if not exists account_quota_windows (
			id integer primary key autoincrement,
			account_key text not null,
			provider text not null,
			provider_window_id text not null,
			window_kind text not null,
			window_mode text not null,
			model_scope_kind text not null,
			model_scope_key text,
			model_ids_json text,
			scope_fingerprint text not null,
			inventory_scope_key text not null,
			relationship_kind text,
			container_provider_window_id text,
			availability text not null,
			generation integer not null default 1,
			absence_count integer not null default 0,
			first_seen_at_ms integer not null,
			last_seen_at_ms integer not null,
			missing_since_ms integer,
			deactivated_at_ms integer,
			last_observation_id integer,
			created_at_ms integer not null,
			updated_at_ms integer not null,
			unique(account_key, provider, provider_window_id, scope_fingerprint),
			foreign key(last_observation_id) references account_quota_observations(id)
		);

CREATE TABLE IF NOT EXISTS analytics_accounts (id TEXT PRIMARY KEY,provider TEXT NOT NULL,label TEXT NOT NULL,auth_index TEXT NOT NULL,auth_file TEXT NOT NULL,last_seen_ms INTEGER NOT NULL,poll_at_ms INTEGER NOT NULL DEFAULT 0,poll_error TEXT NOT NULL DEFAULT '');

CREATE TABLE IF NOT EXISTS analytics_credential_inventory (
 account TEXT PRIMARY KEY, checked_ms INTEGER NOT NULL, status TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS analytics_event_keys (event_hash TEXT PRIMARY KEY,api_key_hash TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS analytics_events (event_hash TEXT PRIMARY KEY,event_id INTEGER NOT NULL,account TEXT NOT NULL,started_ms INTEGER NOT NULL,completed_ms INTEGER NOT NULL,payload TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS analytics_forecast_results (id TEXT PRIMARY KEY,forecast_id TEXT NOT NULL,settled_ms INTEGER NOT NULL,state TEXT NOT NULL,payload TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS analytics_forecast_work (account TEXT PRIMARY KEY,generation INTEGER NOT NULL,next_ms INTEGER NOT NULL,after_forecast INTEGER NOT NULL DEFAULT 0,waiting INTEGER NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS analytics_forecasts (id TEXT PRIMARY KEY,account TEXT NOT NULL,window_id TEXT NOT NULL,issued_ms INTEGER NOT NULL,reset_ms INTEGER NOT NULL,payload TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS analytics_historical_valuations (id TEXT PRIMARY KEY,payload TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS analytics_normalization_rules (
 id TEXT PRIMARY KEY, captured_ms INTEGER NOT NULL, payload TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS analytics_observations (id TEXT PRIMARY KEY,account TEXT NOT NULL,observed_ms INTEGER NOT NULL,window_id TEXT NOT NULL,payload TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS analytics_poll_runs (id INTEGER PRIMARY KEY AUTOINCREMENT,account TEXT NOT NULL,observed_ms INTEGER NOT NULL,source TEXT NOT NULL,status TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS analytics_prices (id TEXT PRIMARY KEY,model TEXT NOT NULL,captured_at_ms INTEGER NOT NULL,payload TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS analytics_profile_revisions (
 revision INTEGER PRIMARY KEY, created_ms INTEGER NOT NULL, action TEXT NOT NULL, payload TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS analytics_state (name TEXT PRIMARY KEY,value INTEGER NOT NULL);

CREATE TABLE IF NOT EXISTS analytics_token_evidence (revision INTEGER PRIMARY KEY AUTOINCREMENT,event_hash TEXT NOT NULL,source_id TEXT NOT NULL,payload TEXT NOT NULL,UNIQUE(event_hash,source_id));

create table if not exists api_key_alias_history (
			api_key_hash text primary key,
			current_hash text not null
		);

create table if not exists api_key_aliases (
			api_key_hash text primary key,
			alias text not null,
			updated_at_ms integer not null
		);

create table if not exists model_price_context_tiers (
			model text not null,
			threshold_tokens integer not null,
			prompt_per_1m real not null default 0,
			completion_per_1m real not null default 0,
			cache_per_1m real not null default 0,
			cache_read_per_1m real not null default 0,
			cache_creation_per_1m real not null default 0,
			prompt_configured integer not null default 0,
			completion_configured integer not null default 0,
			cache_configured integer not null default 0,
			cache_read_configured integer not null default 0,
			cache_creation_configured integer not null default 0,
			primary key (model, threshold_tokens),
			foreign key (model) references model_prices(model) on delete cascade
		);

create table if not exists model_price_service_tiers (
			model text not null,
			mode text not null,
			service_tier text not null,
			prompt_per_1m real not null default 0,
			completion_per_1m real not null default 0,
			cache_per_1m real not null default 0,
			cache_read_per_1m real not null default 0,
			cache_creation_per_1m real not null default 0,
			prompt_configured integer not null default 0,
			completion_configured integer not null default 0,
			cache_configured integer not null default 0,
			cache_read_configured integer not null default 0,
			cache_creation_configured integer not null default 0,
			primary key (model, mode, service_tier),
			foreign key (model) references model_prices(model) on delete cascade
		);

create table if not exists model_prices (
			model text primary key,
			prompt_per_1m real not null,
			completion_per_1m real not null,
			cache_per_1m real not null,
			cache_read_per_1m real not null default 0,
			cache_creation_per_1m real not null default 0,
			prompt_configured integer not null default 0,
			completion_configured integer not null default 0,
			cache_read_configured integer not null default 0,
			cache_creation_configured integer not null default 0,
			source text,
			source_model_id text,
			raw_json text,
			updated_at_ms integer not null,
			synced_at_ms integer
		);

create table if not exists settings (
			key text primary key,
			value text not null,
			updated_at_ms integer not null
		);

create table if not exists usage_event_identity_ledger (
			event_hash text primary key,
			raw_event_id integer,
			timestamp_ms integer not null,
			bucket_ms integer not null,
			aggregate_schema_version integer not null default 0,
			aggregate_structure_revision text not null default '',
			first_seen_at_ms integer not null,
			updated_at_ms integer not null
		);

create table if not exists usage_events (
			id integer primary key autoincrement,
			request_id text,
			event_hash text not null unique,
			timestamp_ms integer not null,
			timestamp text not null,
			provider text,
			executor_type text,
			model text not null,
			endpoint text,
			method text,
			path text,
			client_ip text,
			x_forwarded_for text,
			user_agent text,
			auth_type text,
			auth_index text,
			source text,
			source_hash text,
			api_key_hash text,
			account_snapshot text,
			auth_label_snapshot text,
			auth_file_snapshot text,
			auth_provider_snapshot text,
			auth_account_id_snapshot text,
			auth_project_id_snapshot text,
			auth_snapshot_at_ms integer,
			requested_model text,
			resolved_model text,
			reasoning_effort text,
			service_tier text,
			request_service_tier text,
			response_service_tier text,
			cache_input_mode text,
			input_tokens integer not null default 0,
			output_tokens integer not null default 0,
			reasoning_tokens integer not null default 0,
			cached_tokens integer not null default 0,
			cache_tokens integer not null default 0,
			cache_read_tokens integer not null default 0,
			cache_creation_tokens integer not null default 0,
			normalized_uncached_input_tokens integer,
			normalized_total_input_tokens integer,
			normalized_cache_read_tokens integer,
			normalized_cache_creation_tokens integer,
			total_tokens integer not null default 0,
			latency_ms integer,
			ttft_ms integer,
			failed integer not null default 0,
			fail_status_code integer,
			fail_summary text,
			response_metadata_json text,
			header_quota_recover_at_ms integer,
			header_quota_used_percent real,
			header_quota_plan_type text,
			header_error_kind text,
			header_error_code text,
			header_trace_id text,
			fail_body text,
			raw_json text,
			created_at_ms integer not null
		);
