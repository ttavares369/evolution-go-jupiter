package queuev1

// Existing identities, outcomes and receipts are not rewritten. Empty means no
// deadline, including every row originally admitted under schema v1.
const postgresDeadlineDDL = `
ALTER TABLE queue_v1_operations ADD COLUMN valid_until TEXT NOT NULL DEFAULT '';
ALTER TABLE queue_v1_attempts ADD COLUMN valid_until TEXT NOT NULL DEFAULT '';
ALTER TABLE queue_v1_operations ADD CONSTRAINT queue_v1_operations_deadline_format
 CHECK(valid_until = '' OR valid_until ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z$');
ALTER TABLE queue_v1_operations ADD CONSTRAINT queue_v1_operations_deadline_binding
 UNIQUE(instance_id,logical_id,message_id,content_hash,valid_until);
ALTER TABLE queue_v1_attempts ADD CONSTRAINT queue_v1_attempts_deadline_binding
 FOREIGN KEY(instance_id,logical_id,message_id,content_hash,valid_until)
 REFERENCES queue_v1_operations(instance_id,logical_id,message_id,content_hash,valid_until);
ALTER TABLE queue_v1_attempts DROP CONSTRAINT queue_v1_attempts_check;
ALTER TABLE queue_v1_attempts ADD CONSTRAINT queue_v1_attempts_send_boundary CHECK(
 (state IN ('sending','uncertain','accepted') AND sending_at IS NOT NULL)
 OR (state = 'preparing' AND sending_at IS NULL)
 OR (state = 'rejected' AND (sending_at IS NULL OR (code = 'expired' AND valid_until <> '')))
);
UPDATE queue_v1_schema SET version=2 WHERE singleton=TRUE;
`
