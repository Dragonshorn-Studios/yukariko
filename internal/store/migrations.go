package store

// migration is one forward-only schema step. Never edit an applied migration;
// append a new one instead.
type migration struct {
	version int
	ddl     string
}

// migrations are applied in order, each inside its own transaction recorded
// in schema_migrations.
var migrations = []migration{
	{
		version: 1,
		ddl: `
CREATE TABLE observations (
	app_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	value TEXT NOT NULL,
	observed_at TEXT NOT NULL,
	PRIMARY KEY (app_id, kind)
);

CREATE TABLE deployed_versions (
	app_id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	value TEXT NOT NULL,
	deployment_id TEXT NOT NULL,
	deployed_at TEXT NOT NULL
);

CREATE TABLE deployments (
	id TEXT PRIMARY KEY,
	app_id TEXT NOT NULL,
	cause TEXT NOT NULL,
	from_version TEXT NOT NULL,
	to_version TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('running','succeeded','failed','interrupted')),
	started_at TEXT NOT NULL,
	ended_at TEXT,
	error TEXT
);
CREATE INDEX idx_deployments_app_started ON deployments(app_id, started_at);

CREATE TABLE events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts TEXT NOT NULL,
	app_id TEXT,
	level TEXT NOT NULL,
	kind TEXT NOT NULL,
	message TEXT NOT NULL,
	data TEXT
);
CREATE INDEX idx_events_ts ON events(ts);
CREATE INDEX idx_events_app_ts ON events(app_id, ts);

CREATE TABLE command_runs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	deployment_id TEXT,
	app_id TEXT,
	name TEXT NOT NULL,
	argv TEXT NOT NULL,
	dir TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('success','failed','timeout','cancelled')),
	exit_code INTEGER,
	started_at TEXT NOT NULL,
	ended_at TEXT NOT NULL,
	stdout_excerpt TEXT,
	stderr_excerpt TEXT,
	stdout_bytes INTEGER NOT NULL DEFAULT 0,
	stderr_bytes INTEGER NOT NULL DEFAULT 0,
	truncated INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_command_runs_deployment ON command_runs(deployment_id);

CREATE TABLE health_samples (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	app_id TEXT NOT NULL,
	check_kind TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('checking','healthy','unhealthy','unknown')),
	reason TEXT,
	latency_ms INTEGER,
	checked_at TEXT NOT NULL
);
CREATE INDEX idx_health_samples_app_time ON health_samples(app_id, checked_at);

CREATE TABLE health_current (
	app_id TEXT NOT NULL,
	check_kind TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('checking','healthy','unhealthy','unknown')),
	reason TEXT,
	checked_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (app_id, check_kind)
);

CREATE TABLE outbound_events (
	id TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	available_at TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	state TEXT NOT NULL CHECK (state IN ('pending','delivered')),
	coalesce_key TEXT,
	kind TEXT NOT NULL,
	payload TEXT NOT NULL,
	last_error TEXT
);
CREATE INDEX idx_outbound_pending ON outbound_events(state, available_at);

CREATE TABLE delivery_attempts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id TEXT NOT NULL REFERENCES outbound_events(id) ON DELETE CASCADE,
	attempted_at TEXT NOT NULL,
	result TEXT NOT NULL,
	status_code INTEGER,
	error TEXT
);
CREATE INDEX idx_delivery_attempts_event ON delivery_attempts(event_id);

CREATE TABLE inbound_event_ids (
	host_id TEXT NOT NULL,
	event_id TEXT NOT NULL,
	received_at TEXT NOT NULL,
	PRIMARY KEY (host_id, event_id)
);

CREATE TABLE remote_hosts (
	host_id TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	last_seen_at TEXT NOT NULL
);

CREATE TABLE remote_state (
	host_id TEXT NOT NULL,
	app_id TEXT NOT NULL,
	data TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (host_id, app_id)
);
`,
	},
}
