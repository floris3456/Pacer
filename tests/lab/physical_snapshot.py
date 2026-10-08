"""Read-only native-lab cut wait and SQLite backup; never a coverage authority.

Run in the owned network-none inspector with /state:ro and private /snapshots.
The caller must first verify runtime ownership, versions and exclusive lab use.
"""
import json
import os
import pathlib
import sqlite3
import sys
import time


def inspect(connection, consumer):
    def rows(sql, args=(consumer,)):
        return connection.execute(sql, args).fetchall()

    def bodies(sql):
        result = rows(sql + " LIMIT 4097")
        assert len(result) <= 4096, "bounded lab snapshot exceeded"
        return [json.loads(row[0]) for row in result]

    return {
        "consumer_id": consumer,
        "current_faults": rows("SELECT count(*) FROM measurement_physical_faults WHERE consumer_id=?")[0][0],
        "old_faults_retained": rows("SELECT count(*) FROM measurement_physical_faults WHERE consumer_id!=?")[0][0],
        "current_origin_gaps": rows("SELECT count(*) FROM measurement_physical_gaps g WHERE EXISTS(SELECT 1 FROM measurement_physical_epochs e WHERE e.consumer_id=? AND e.source_ref=g.source_ref AND e.manager_id=g.manager_id)")[0][0],
        "records": bodies("SELECT p.body FROM measurement_physical_records p JOIN measurement_physical_streams s ON s.id=p.stream_id WHERE s.consumer_id=? ORDER BY p.dispatch_seq"),
        "lower_cuts": bodies("SELECT lower_body FROM measurement_physical_epochs WHERE consumer_id=? AND lower_body IS NOT NULL"),
        "proofs": bodies("SELECT p.body FROM measurement_physical_proofs p JOIN measurement_physical_epochs e ON e.id=p.epoch_id WHERE e.consumer_id=?"),
        "streams": rows("SELECT count(*) FROM measurement_physical_streams WHERE consumer_id=?")[0][0],
        "streams_waiting_upper_cut": rows("SELECT count(*) FROM measurement_physical_streams s WHERE s.consumer_id=? AND EXISTS(SELECT 1 FROM measurement_physical_records r WHERE r.stream_id=s.id AND r.dispatch_seq>=coalesce((SELECT max(p.upper_seq) FROM measurement_physical_proofs p JOIN measurement_physical_epochs e ON e.id=p.epoch_id WHERE e.consumer_id=s.consumer_id AND e.source_ref=s.source_ref AND e.manager_id=s.manager_id),0))")[0][0],
        "forward_feed_absent": not rows("SELECT count(*) FROM sqlite_master WHERE name=?", ("measurement_report_meta",))[0][0],
    }


def ready(phase, result):
    if phase == "anchor":
        return len(result["records"]) >= 2 and result["streams"] > 0 and len(result["lower_cuts"]) >= result["streams"]
    if phase == "positive":
        return len(result["records"]) >= 5 and not result["streams_waiting_upper_cut"] and bool(result["proofs"])
    return result["current_origin_gaps"] > 0


def main():
    phase, name = sys.argv[1:]
    assert phase in ("anchor", "positive", "revoked")
    assert pathlib.Path(name).name == name and name.endswith(".sqlite") and name != "pacer.sqlite"
    path = pathlib.Path("/snapshots") / name
    connection = sqlite3.connect("file:/state/pacer.sqlite?mode=ro", uri=True, timeout=3)
    connection.execute("PRAGMA query_only=ON")
    consumers = connection.execute("SELECT id FROM measurement_source_consumers WHERE active=1").fetchall()
    assert len(consumers) == 1
    consumer = consumers[0][0]
    deadline = time.monotonic() + 70
    while True:
        connection.execute("BEGIN")
        result = inspect(connection, consumer)
        connection.rollback()
        if result["current_faults"] or ready(phase, result) or time.monotonic() >= deadline:
            break
        time.sleep(.25)
    # Exclusive private output; keep even an unsuccessful snapshot as evidence.
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    os.close(fd)
    target = sqlite3.connect(path)
    connection.backup(target)
    # Report facts from the saved copy, not a different moving live read.
    result = inspect(target, consumer)
    target.close()
    connection.close()
    result["phase"] = phase
    print(json.dumps(result, indent=2))
    assert ready(phase, result) and not result["current_faults"]
    assert phase == "revoked" or not result["current_origin_gaps"]


if __name__ == "__main__":
    main()
