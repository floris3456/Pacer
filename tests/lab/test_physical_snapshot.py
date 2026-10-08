import sqlite3
import unittest

from physical_snapshot import inspect, ready


class PhysicalSnapshotTest(unittest.TestCase):
    def test_inspection_and_explicit_cut_wait(self):
        db = sqlite3.connect(":memory:")
        self.addCleanup(db.close)
        db.executescript("""
        CREATE TABLE measurement_physical_faults(consumer_id TEXT);
        CREATE TABLE measurement_physical_gaps(source_ref TEXT,manager_id TEXT);
        CREATE TABLE measurement_physical_epochs(id TEXT,consumer_id TEXT,source_ref TEXT,manager_id TEXT,lower_body TEXT);
        CREATE TABLE measurement_physical_streams(id TEXT,consumer_id TEXT,source_ref TEXT,manager_id TEXT);
        CREATE TABLE measurement_physical_records(stream_id TEXT,dispatch_seq INTEGER,body TEXT);
        CREATE TABLE measurement_physical_proofs(epoch_id TEXT,upper_seq INTEGER,body TEXT);
        INSERT INTO measurement_physical_faults VALUES('old');
        INSERT INTO measurement_physical_epochs VALUES('e','current','source','manager',NULL);
        INSERT INTO measurement_physical_streams VALUES('s','current','source','manager');
        INSERT INTO measurement_physical_records VALUES('s',1,'{}'),('s',2,'{}');
        """)
        first = inspect(db, "current")
        self.assertTrue(first["forward_feed_absent"])
        self.assertEqual(first["old_faults_retained"], 1)
        self.assertEqual(first["current_faults"], 0)
        self.assertFalse(ready("anchor", first))
        db.execute("UPDATE measurement_physical_epochs SET lower_body='{}'")
        self.assertTrue(ready("anchor", inspect(db, "current")))
        db.executemany("INSERT INTO measurement_physical_records VALUES('s',?,'{}')", [(3,), (4,), (5,)])
        self.assertFalse(ready("positive", inspect(db, "current")))
        db.execute("INSERT INTO measurement_physical_proofs VALUES('e',5,'{}')")
        self.assertFalse(ready("positive", inspect(db, "current")))
        db.execute("UPDATE measurement_physical_proofs SET upper_seq=6")
        self.assertTrue(ready("positive", inspect(db, "current")))
        self.assertFalse(ready("revoked", inspect(db, "current")))
        db.execute("INSERT INTO measurement_physical_gaps VALUES('source','manager')")
        self.assertTrue(ready("revoked", inspect(db, "current")))
        # Same SQL also works with a read-only connection policy; the original
        # inline helper's malformed schema-name quoting is covered by this query.
        db.execute("CREATE TABLE measurement_report_meta(id INTEGER)")
        db.execute("PRAGMA query_only=ON")
        self.assertFalse(inspect(db, "current")["forward_feed_absent"])


if __name__ == "__main__":
    unittest.main()
