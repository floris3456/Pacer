package measurement

const sourceSchema = `
CREATE TABLE measurement_source_limits (id INTEGER PRIMARY KEY CHECK(id=1), consumers INTEGER NOT NULL, memberships INTEGER NOT NULL, bindings INTEGER NOT NULL, runs INTEGER NOT NULL, epochs INTEGER NOT NULL, gaps INTEGER NOT NULL, faults INTEGER NOT NULL);
INSERT INTO measurement_source_limits VALUES(1,0,0,0,0,0,0,0);
CREATE TABLE measurement_source_consumers (id TEXT PRIMARY KEY, source_id TEXT NOT NULL, started_at INTEGER NOT NULL, active INTEGER NOT NULL, poisoned INTEGER NOT NULL);
CREATE UNIQUE INDEX measurement_source_active_consumer ON measurement_source_consumers(source_id) WHERE active=1;
CREATE TABLE measurement_source_memberships (id TEXT PRIMARY KEY, source_id TEXT NOT NULL, provider TEXT NOT NULL, account_id TEXT NOT NULL, revision INTEGER NOT NULL, cut_at INTEGER NOT NULL, body BLOB NOT NULL);
CREATE UNIQUE INDEX measurement_source_membership_revision ON measurement_source_memberships(source_id,provider,account_id,revision);
CREATE TABLE measurement_source_membership_heads (source_id TEXT NOT NULL, provider TEXT NOT NULL, account_id TEXT NOT NULL, id TEXT NOT NULL, PRIMARY KEY(source_id,provider,account_id));
CREATE TABLE measurement_source_bindings (source_ref TEXT PRIMARY KEY, source_id TEXT NOT NULL, provider TEXT NOT NULL, account_id TEXT NOT NULL, credential_ref TEXT NOT NULL);
CREATE TABLE measurement_source_runs (consumer_id TEXT NOT NULL, membership_id TEXT NOT NULL, source_ref TEXT NOT NULL, manager_id TEXT NOT NULL, sink_id TEXT NOT NULL, active INTEGER NOT NULL, body BLOB NOT NULL, PRIMARY KEY(consumer_id,membership_id,source_ref,manager_id,sink_id));
CREATE UNIQUE INDEX measurement_source_active_run ON measurement_source_runs(consumer_id,membership_id,source_ref) WHERE active=1;
CREATE TABLE measurement_source_epochs (source_ref TEXT NOT NULL, manager_id TEXT NOT NULL, sink_id TEXT NOT NULL, active INTEGER NOT NULL, body BLOB NOT NULL, PRIMARY KEY(source_ref,manager_id,sink_id));
CREATE UNIQUE INDEX measurement_source_active_epoch ON measurement_source_epochs(source_ref) WHERE active=1;
CREATE TABLE measurement_source_gaps (source_ref TEXT NOT NULL, manager_id TEXT NOT NULL, sink_id TEXT NOT NULL, invalid_from INTEGER NOT NULL, PRIMARY KEY(source_ref,manager_id,sink_id));
CREATE TABLE measurement_source_faults (consumer_id TEXT NOT NULL, account_id TEXT NOT NULL, invalid_from INTEGER NOT NULL, PRIMARY KEY(consumer_id,account_id));
UPDATE measurement_meta SET version=2 WHERE id=1;
`

var sourceTables = []string{"measurement_source_limits", "measurement_source_consumers", "measurement_source_memberships", "measurement_source_membership_heads", "measurement_source_bindings", "measurement_source_runs", "measurement_source_epochs", "measurement_source_gaps", "measurement_source_faults"}
