package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// errPrerelease means the proposed pin is a pre-release; the nightly workflow skips it.
var errPrerelease = errors.New("the new pin is a pre-release; not proposed")

var (
	slimTagRe   = regexp.MustCompile(`^[a-z][a-z0-9-]*-v?\d[0-9A-Za-z.+-]*-r\d+$`)
	studioTagRe = regexp.MustCompile(`^\d{4}\.\d{2}\.\d{2}-sha-[0-9a-f]{7,40}$`)
	preRe       = regexp.MustCompile(`(?i)[-.](rc|beta|alpha|pre|dev|preview|canary|next)[-.0-9]`)
)

// Bump returns src (the text of internal/versions/versions.yaml) with the pin of service moved
// to `to`, leaving every comment and every other line as it was. service is an artifact name, or
// "studio" for studio.tag. For an artifact `to` is the slim-services tag and must belong to that
// service; a bump to a pre-release is refused with errPrerelease.
func Bump(src []byte, service, to string) ([]byte, error) {
	text := string(src)
	if service == "studio" {
		if !studioTagRe.MatchString(to) {
			return nil, fmt.Errorf("studio pin %q is not <date>-sha-<commit>", to)
		}
		re := regexp.MustCompile(`(?m)^(  tag:[ \t]+)(\S+)([ \t]*)$`)
		m := re.FindStringSubmatch(text)
		if m == nil {
			return nil, errors.New("versions.yaml has no studio.tag line")
		}
		old := m[2]
		if old == to {
			return src, nil
		}
		text = re.ReplaceAllString(text, "${1}"+to+"${3}")
		// The comment above the pin names the slim artifact of the same version.
		text = strings.ReplaceAll(text, "studio-"+old+"-r", "studio-"+to+"-r")
		return []byte(text), nil
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9-]*$`).MatchString(service) {
		return nil, fmt.Errorf("service %q is not an artifact name", service)
	}
	if !slimTagRe.MatchString(to) || !strings.HasPrefix(to, service+"-") {
		return nil, fmt.Errorf("pin %q is not a %s-<version>-r<N> tag of %s", to, service, service)
	}
	if preRe.MatchString(strings.TrimPrefix(to, service)) {
		return nil, errPrerelease
	}
	re := regexp.MustCompile(`(?m)^(  ` + regexp.QuoteMeta(service) + `:[ \t]+)(\S+)([ \t]*)$`)
	if !re.MatchString(text) {
		return nil, fmt.Errorf("versions.yaml has no artifacts.%s line", service)
	}
	return []byte(re.ReplaceAllString(text, "${1}"+to+"${3}")), nil
}
