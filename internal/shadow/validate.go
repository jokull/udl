package shadow

import (
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("shadow: invalid name %q (use 1-64 letters, digits, hyphens or underscores)", name)
	}
	return nil
}

func within(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator))
}

// Validate rejects ambiguous exports before any service or mount is changed.
func Validate(defs []Def) error {
	names, ports := map[string]bool{}, map[int]bool{}
	for i, d := range defs {
		if err := ValidateName(d.Name); err != nil {
			return err
		}
		if names[strings.ToLower(d.Name)] || (d.Type != "movie" && d.Type != "show") {
			return fmt.Errorf("shadow %q: duplicate name or invalid media type", d.Name)
		}
		names[strings.ToLower(d.Name)] = true
		if !filepath.IsAbs(d.Mount) || filepath.Clean(d.Mount) == "/" {
			return fmt.Errorf("shadow %q: mount must be an absolute non-root path", d.Name)
		}
		if d.Upper != "" && (!filepath.IsAbs(d.Upper) || filepath.Clean(d.Upper) == "/" || within(d.Upper, d.Mount) || within(d.Mount, d.Upper)) {
			return fmt.Errorf("shadow %q: upper must be absolute and separate from the mount", d.Name)
		}
		if d.Port < 0 || d.Port > 65535 || (d.Port != 0 && ports[d.Port]) {
			return fmt.Errorf("shadow %q: invalid or duplicate NFS port %d", d.Name, d.Port)
		}
		ports[d.Port] = true
		for _, prior := range defs[:i] {
			if within(d.Mount, prior.Mount) || within(prior.Mount, d.Mount) {
				return fmt.Errorf("shadow %q: mount overlaps %q", d.Name, prior.Name)
			}
		}
	}
	return nil
}

func ValidateManifest(m *Manifest) error {
	if err := ValidateName(m.Name); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, it := range m.Items {
		p := norm.NFD.String(it.VirtualPath)
		u, err := url.Parse(it.URL)
		if p == "" || p == "." || path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") || strings.ContainsRune(p, '\x00') || seen[p] {
			return fmt.Errorf("shadow: invalid or duplicate virtual path %q", it.VirtualPath)
		}
		if it.Size <= 0 || err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("shadow: invalid size or source for %q", it.VirtualPath)
		}
		seen[p] = true
	}
	for p := range seen {
		for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return fmt.Errorf("shadow: file/directory collision at %q", parent)
			}
		}
	}
	return nil
}
