package runtimedb

// The household's banner photo.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MaxBannerBytes bounds one banner.
//
// Four megabytes: comfortably a phone photograph, and small enough that
// the row is read into memory on every page load without thinking about
// it. A banner larger than this is a picture nobody wanted at that size
// — the browser is going to scale it into a 172px-tall band either way.
const MaxBannerBytes = 4 << 20

// HouseholdBanner is the picture and its framing.
type HouseholdBanner struct {
	Image     []byte `json:"-"`
	MediaType string `json:"mediaType"`
	// PositionX and PositionY are CSS object-position percentages: which
	// part of the photograph the band shows.
	PositionX float64   `json:"positionX"`
	PositionY float64   `json:"positionY"`
	SetBy     string    `json:"setBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SetHouseholdBanner replaces the picture, resetting the framing to
// centre.
//
// The framing resets deliberately. A position chosen for one photograph
// means nothing about the next one, and carrying it over would show
// somebody a new picture already cropped somewhere they did not choose.
func (db *DB) SetHouseholdBanner(image []byte, mediaType, by string) error {
	switch {
	case len(image) == 0:
		return fmt.Errorf("runtimedb: a banner needs an image")
	case len(image) > MaxBannerBytes:
		return fmt.Errorf("runtimedb: that picture is larger than %d bytes", MaxBannerBytes)
	case mediaType == "":
		return fmt.Errorf("runtimedb: a banner needs a media type")
	}
	_, err := db.sql.Exec(
		`INSERT INTO household_banner (id, image, media_type, position_x, position_y, set_by, updated_at)
		 VALUES (1, ?, ?, 50, 50, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET
		   image = excluded.image, media_type = excluded.media_type,
		   position_x = 50, position_y = 50,
		   set_by = excluded.set_by, updated_at = excluded.updated_at`,
		image, mediaType, by, now())
	if err != nil {
		return fmt.Errorf("runtimedb: setting the household banner: %w", err)
	}
	return nil
}

// SetHouseholdBannerPosition moves the framing without touching the
// picture. Percentages are clamped rather than refused: a drag that runs
// past the edge means "as far as it goes", not "that was invalid".
func (db *DB) SetHouseholdBannerPosition(x, y float64, by string) error {
	res, err := db.sql.Exec(
		`UPDATE household_banner SET position_x = ?, position_y = ?, set_by = ?, updated_at = ? WHERE id = 1`,
		clampPercent(x), clampPercent(y), by, now())
	if err != nil {
		return fmt.Errorf("runtimedb: framing the household banner: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// GetHouseholdBanner returns the picture, or ErrNotFound when the
// household has not set one.
func (db *DB) GetHouseholdBanner() (HouseholdBanner, error) {
	var b HouseholdBanner
	var updated string
	err := db.sql.QueryRow(
		`SELECT image, media_type, position_x, position_y, set_by, updated_at
		 FROM household_banner WHERE id = 1`).
		Scan(&b.Image, &b.MediaType, &b.PositionX, &b.PositionY, &b.SetBy, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return HouseholdBanner{}, ErrNotFound
	}
	if err != nil {
		return HouseholdBanner{}, fmt.Errorf("runtimedb: reading the household banner: %w", err)
	}
	b.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return b, nil
}

// ClearHouseholdBanner removes the picture, putting the band back to the
// one that ships. Absent is not an error: the control is "use the
// default one", and pressing it twice is not a mistake.
func (db *DB) ClearHouseholdBanner() error {
	if _, err := db.sql.Exec(`DELETE FROM household_banner WHERE id = 1`); err != nil {
		return fmt.Errorf("runtimedb: clearing the household banner: %w", err)
	}
	return nil
}
