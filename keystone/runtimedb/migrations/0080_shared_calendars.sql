-- Keep existing event links in their original Household collection.
ALTER TABLE commitment_bookings ADD COLUMN calendar_id TEXT NOT NULL DEFAULT 'household';
CREATE TABLE calendar_visibility (
 member TEXT NOT NULL,
 calendar_id TEXT NOT NULL,
 visible INTEGER NOT NULL CHECK (visible IN (0,1)),
 PRIMARY KEY (member, calendar_id)
);
