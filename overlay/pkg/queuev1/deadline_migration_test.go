package queuev1

import (
	"context"
	"database/sql"
	"net/url"
	"testing"
	"time"
)

func TestDeadlineRealUpgradePreservesV1EvidenceWithoutInventingDeadline(t *testing.T) {
	_, admin, raw := postgresReal(t)
	schema := "deadline_upgrade_" + postgresID()
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(postgresVersionDDL + postgresTablesDDL); err != nil {
		t.Fatal("create original v1 schema", err)
	}
	b := postgresBinding("original-instance")
	if _, err = db.Exec(`INSERT INTO queue_v1_instances(instance_id) VALUES($1)`, b.InstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO queue_v1_operations(instance_id,logical_id,message_id,content_hash,state,current_attempt_id) VALUES($1,$2,$3,$4,'accepted',$5)`, b.InstanceID, b.LogicalMessageID, b.MessageID, b.ContentHash, b.AttemptID); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if _, err = db.Exec(`INSERT INTO queue_v1_attempts(instance_id,attempt_id,logical_id,message_id,content_hash,owner_id,state,created_at,sending_at,completed_at,receipt_message_id,receipt_accepted_at) VALUES($1,$2,$3,$4,$5,$6,'accepted',$7,$7,$7,$4,$7)`, b.InstanceID, b.AttemptID, b.LogicalMessageID, b.MessageID, b.ContentHash, b.OwnerID, at); err != nil {
		t.Fatal(err)
	}
	p, _ := NewPostgresLedger(db)
	for range 2 {
		if err = p.Migrate(context.Background()); err != nil {
			t.Fatal("upgrade/reopen", err)
		}
	}
	a, err := p.Lookup(context.Background(), b.InstanceID, b.AttemptID)
	if err != nil || a.State != Accepted || a.ValidUntil != "" || a.ContentHash != b.ContentHash || a.Receipt == nil || !a.Receipt.AcceptedAt.Equal(at) {
		t.Fatalf("v1 evidence changed: %+v %v", a, err)
	}
	var version int
	if err = db.QueryRow(`SELECT version FROM queue_v1_schema`).Scan(&version); err != nil || version != 2 {
		t.Fatal("schema version not upgraded")
	}
	if _, err = db.Exec(`UPDATE queue_v1_attempts SET valid_until='2026-11-05T13:00:00.000Z' WHERE instance_id=$1`, b.InstanceID); err == nil {
		t.Fatal("attempt deadline changed independently of binding")
	}
}
