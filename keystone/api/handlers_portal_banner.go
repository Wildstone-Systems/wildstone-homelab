package api

// The household's banner photo: setting it, framing it, and serving it
// back.
//
// THE ONE PLACE A HOUSEHOLD SURFACE ACCEPTS AN IMAGE, and the whole of
// why it is careful. What comes in is bytes a member chose, and what
// goes out is served from the same origin as the pages that hold
// everybody's session — so the media type is decided HERE, from the
// bytes themselves, and never from what the upload claimed. A caller
// that sends an HTML file with an image content-type is describing a
// stored XSS, and the only reliable answer is not to believe them.
//
// THE FRAMING IS A SEPARATE WRITE. A photograph is arbitrary and a
// banner is a wide, short crop of one, so "which part of it do you
// want to see" is a real question — and it is asked after the picture
// is already on screen, where a person can see what they are choosing,
// rather than as a field on the upload form where they would be
// guessing.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// homeBannerSet stores a new picture.
//
// The body IS the image, as the filedrop upload's body is the file and
// for the same reason: a multipart request carries the caller's own
// description of what it sent, and here that description decides
// nothing anyway.
func (s *Server) homeBannerSet(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this page is not ready yet")
	}
	// The declared length, refused before the body is read, so a phone
	// on a slow connection is told immediately rather than after sending
	// a photograph.
	if r.ContentLength > runtimedb.MaxBannerBytes {
		return nil, http.StatusRequestEntityTooLarge,
			fmt.Errorf("that picture is too big — the limit is %d MB", runtimedb.MaxBannerBytes>>20)
	}
	defer r.Body.Close()
	// One byte over the limit is still read, so that a body which lied
	// about its length is caught by the same bound rather than by
	// trusting Content-Length twice.
	image, err := io.ReadAll(io.LimitReader(r.Body, runtimedb.MaxBannerBytes+1))
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("that picture didn't arrive in one piece")
	}
	if len(image) > runtimedb.MaxBannerBytes {
		return nil, http.StatusRequestEntityTooLarge,
			fmt.Errorf("that picture is too big — the limit is %d MB", runtimedb.MaxBannerBytes>>20)
	}
	// The media type comes from the BYTES, never from the request. An
	// upload that is not one of the formats a browser will draw is
	// refused rather than stored and served back as something the
	// browser has to guess about.
	mediaType, ok := portal.ImageType(image)
	if !ok {
		return nil, http.StatusUnsupportedMediaType,
			fmt.Errorf("that isn't a picture this can use — try a JPEG, PNG, WebP or GIF")
	}
	if err := s.DB.SetHouseholdBanner(image, mediaType, member); err != nil {
		s.Log.Error("household banner", "err", err)
		return nil, http.StatusInternalServerError, fmt.Errorf("that didn't save")
	}
	s.event("household.banner", "", principal.Identity{Name: portalPrincipal, ActingFor: member},
		fmt.Sprintf("%s changed the dashboard picture", member))
	return map[string]any{"mediaType": mediaType, "positionX": 50.0, "positionY": 50.0}, http.StatusOK, nil
}

// homeBannerFrame moves which part of the picture the band shows.
func (s *Server) homeBannerFrame(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this page is not ready yet")
	}
	x, xerr := strconv.ParseFloat(strings.TrimSpace(r.URL.Query().Get("x")), 64)
	y, yerr := strconv.ParseFloat(strings.TrimSpace(r.URL.Query().Get("y")), 64)
	if xerr != nil || yerr != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("where in the picture?")
	}
	if err := s.DB.SetHouseholdBannerPosition(x, y, member); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("there's no picture to frame yet")
		}
		s.Log.Error("household banner frame", "err", err)
		return nil, http.StatusInternalServerError, fmt.Errorf("that didn't save")
	}
	return map[string]any{"positionX": x, "positionY": y}, http.StatusOK, nil
}

// homeBannerClear puts the band back to the picture that ships.
func (s *Server) homeBannerClear(_ http.ResponseWriter, _ *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this page is not ready yet")
	}
	if err := s.DB.ClearHouseholdBanner(); err != nil {
		s.Log.Error("household banner clear", "err", err)
		return nil, http.StatusInternalServerError, fmt.Errorf("that didn't save")
	}
	s.event("household.banner", "", principal.Identity{Name: portalPrincipal, ActingFor: member},
		fmt.Sprintf("%s put the dashboard picture back to the default", member))
	return map[string]any{"cleared": true}, http.StatusOK, nil
}

// homeBannerImage serves the picture itself.
//
// NOT CACHED, unlike the design assets beside it. Those never change
// under their name; this one changes the moment somebody uploads
// another, and a year-long immutable cache would leave the household
// looking at the old picture until they cleared their browser.
func (s *Server) homeBannerImage(w http.ResponseWriter, _ *http.Request, _ string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusNotFound, fmt.Errorf("no picture")
	}
	b, err := s.DB.GetHouseholdBanner()
	if err != nil {
		// Absent is a 404 and the page falls back to the bundled
		// picture, which is a household that has not chosen one rather
		// than an error.
		return nil, http.StatusNotFound, fmt.Errorf("no picture")
	}
	w.Header().Set("Content-Type", b.MediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A photograph of somebody's house is not something a shared device
	// should keep after the session ends.
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b.Image)
	return nil, http.StatusOK, nil
}

// bannerView is what the summary carries so the page can draw the band
// on its first paint, without a second request deciding whether there is
// a picture at all.
type bannerView struct {
	// Custom is whether the household has chosen one. False means the
	// page uses the bundled default.
	Custom    bool    `json:"custom"`
	PositionX float64 `json:"positionX"`
	PositionY float64 `json:"positionY"`
	// Version changes whenever the picture does, and the page hangs it
	// on the image URL. Without it a browser holding the previous photo
	// keeps showing it: same URL, same bytes as far as it knows.
	Version int64 `json:"version,omitempty"`
}

// bannerFor reads the household's banner for the summary.
func (s *Server) bannerFor() bannerView {
	if s.DB == nil {
		return bannerView{PositionX: 50, PositionY: 50}
	}
	b, err := s.DB.GetHouseholdBanner()
	if err != nil {
		return bannerView{PositionX: 50, PositionY: 50}
	}
	return bannerView{
		Custom:    true,
		PositionX: b.PositionX,
		PositionY: b.PositionY,
		Version:   b.UpdatedAt.UnixNano(),
	}
}
