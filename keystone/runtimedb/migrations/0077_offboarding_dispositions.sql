-- Every durable resource associated with a departing Person gets an
-- explicit recorded outcome. This table is metadata, never secret material.
CREATE TABLE offboarding_resource_dispositions (
 person_id TEXT NOT NULL REFERENCES butler_members(person_id),
 resource_kind TEXT NOT NULL,
 resource_ref TEXT NOT NULL,
 disposition TEXT NOT NULL CHECK(disposition IN ('transferred','retained','retained-paused','retained-quarantined','blocked','retired','deleted')),
 decided_at TEXT NOT NULL,
 decided_by TEXT NOT NULL,
 PRIMARY KEY(person_id,resource_kind,resource_ref)
);
CREATE INDEX offboarding_resource_dispositions_person ON offboarding_resource_dispositions(person_id,resource_kind);
