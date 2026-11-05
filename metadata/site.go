package metadata

import (
	"errors"
	"net/url"
	"path"
	"strings"

	"github.com/holon-sh/protocol"
)

const Environment = "HOLON_SITE_METADATA"

type Media struct {
	URL    string `json:"url"`
	Type   string `json:"type,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

type Site struct {
	Name           string `json:"name,omitempty"`
	Icon           string `json:"icon,omitempty"`
	AppleTouchIcon string `json:"appleTouchIcon,omitempty"`
	Image          *Media `json:"image,omitempty"`
	Video          *Media `json:"video,omitempty"`
	ThemeColor     string `json:"themeColor,omitempty"`
	DarkThemeColor string `json:"darkThemeColor,omitempty"`
	PublicPreviews bool   `json:"publicPreviews,omitempty"`
}

type Runtime struct {
	Site        Site   `json:"site"`
	Origin      string `json:"origin"`
	AssetOrigin string `json:"assetOrigin,omitempty"`
	Mount       string `json:"mount"`
}

func Parse(raw string) (*Site, error) {
	if raw == "" {
		return nil, nil
	}
	var site Site
	if err := protocol.DecodeJSONNonNull([]byte(raw), &site); err != nil {
		return nil, err
	}
	return &site, site.Validate()
}

func (site Site) Validate() error {
	for _, value := range []string{site.Name, site.ThemeColor, site.DarkThemeColor} {
		if len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("site metadata text must be at most 256 bytes without control characters")
		}
	}
	assets := []string{site.Icon, site.AppleTouchIcon}
	for _, media := range []*Media{site.Image, site.Video} {
		if media == nil {
			continue
		}
		if media.URL == "" || media.Width < 0 || media.Height < 0 || len(media.Type) > 128 || strings.ContainsAny(media.Type, "\x00\r\n") {
			return errors.New("invalid site media")
		}
		assets = append(assets, media.URL)
	}
	for _, raw := range assets {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || len(raw) > 2048 || u.User != nil || strings.ContainsAny(raw, "\\\x00\r\n") ||
			!(u.Scheme == "https" && u.Hostname() != "" || u.Scheme == "" && u.Host == "" && strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//")) {
			return errors.New("site assets require HTTPS URLs or origin-relative paths")
		}
	}
	return nil
}

func (settings Runtime) Validate() error {
	origin, err := protocol.CanonicalOrigin(settings.Origin)
	if err != nil || origin != settings.Origin || !strings.HasPrefix(settings.Mount, "/") || path.Clean(settings.Mount) != settings.Mount || strings.ContainsAny(settings.Mount, "?#\\\x00\r\n") {
		return errors.New("invalid site metadata origin or mount")
	}
	if settings.AssetOrigin != "" {
		origin, err := protocol.CanonicalOrigin(settings.AssetOrigin)
		if err != nil || origin != settings.AssetOrigin {
			return errors.New("invalid site asset origin")
		}
	}
	return settings.Site.Validate()
}

func (settings Runtime) PageURL(requestPath string) string {
	page := url.URL{Path: strings.TrimSuffix(settings.Mount, "/") + "/" + strings.TrimPrefix(requestPath, "/")}
	return settings.Origin + page.EscapedPath()
}

func (settings Runtime) asset(raw string) string {
	if strings.HasPrefix(raw, "/") {
		if settings.AssetOrigin != "" {
			return settings.AssetOrigin + raw
		}
		return settings.Origin + raw
	}
	return raw
}
