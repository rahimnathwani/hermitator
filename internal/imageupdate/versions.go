package imageupdate

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The pins image/Containerfile takes as build arguments, in the order
// changes are reported. RELEASED_BEFORE moves every day, so it is set at
// each build but never on its own a reason for one.
const (
	pinClaude   = "CLAUDE_CODE_VERSION"
	pinPi       = "PI_VERSION"
	pinNode     = "PLAYWRIGHT_VERSION"
	pinPython   = "PLAYWRIGHT_PYTHON_VERSION"
	pinReleased = "RELEASED_BEFORE"
)

var packagePins = []string{pinClaude, pinPi, pinNode, pinPython}

var pinNames = map[string]string{
	pinClaude: "Claude Code",
	pinPi:     "pi",
	pinNode:   "Playwright",
	pinPython: "Playwright for Python",
}

// Pins are build arguments by name.
type Pins map[string]string

// ReadPins reads KEY=VALUE lines: an image's VERSIONS file, or a
// Containerfile's ARG lines (with "ARG " in front).
func ReadPins(path string) (Pins, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := Pins{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "ARG "))
		k, v, ok := strings.Cut(line, "=")
		if ok && k != "" && !strings.ContainsAny(k, " \t#") {
			out[k] = v
		}
	}
	return out, sc.Err()
}

// later reports whether version a comes after b, comparing dotted numbers
// part by part. Timestamps compare as strings, which RFC 3339 allows.
func later(a, b string) bool {
	if !isVersion(a) || !isVersion(b) {
		return a > b
	}
	x, y := parts(a), parts(b)
	for i := range max(len(x), len(y)) {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			return p > q
		}
	}
	return false
}

var (
	stable = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	dotted = regexp.MustCompile(`^\d+(\.\d+)*$`)
)

func isVersion(v string) bool { return dotted.MatchString(v) }

func parts(v string) []int {
	var out []int
	for p := range strings.SplitSeq(v, ".") {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}
	return out
}

// newest is the latest stable version in versions, or "".
func newest(versions []string) string {
	var best string
	for _, v := range versions {
		if best == "" || later(v, best) {
			best = v
		}
	}
	return best
}

// registry looks up releases, as deploy/bump-image.sh does.
type registry struct {
	npm, pypi string
	client    *http.Client
}

func (r registry) getJSON(ctx context.Context, url string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// npmSettled is the newest stable release of an npm package published
// before cutoff, within a minor version ("1.64") if given.
func (r registry) npmSettled(ctx context.Context, pkg string, cutoff time.Time, minor string) (string, error) {
	var doc struct {
		Time map[string]string `json:"time"`
	}
	if err := r.getJSON(ctx, r.npm+"/"+strings.ReplaceAll(pkg, "/", "%2f"), &doc); err != nil {
		return "", err
	}
	var ok []string
	for v, at := range doc.Time {
		t, err := time.Parse(time.RFC3339, at)
		if err != nil || !stable.MatchString(v) || t.After(cutoff) {
			continue
		}
		if minor != "" && !strings.HasPrefix(v, minor+".") {
			continue
		}
		ok = append(ok, v)
	}
	if len(ok) == 0 {
		return "", fmt.Errorf("no release of %s before %s", pkg, cutoff.Format(time.DateOnly))
	}
	return newest(ok), nil
}

// pypiSettled is the newest stable release of a PyPI package whose files
// were all uploaded before cutoff.
func (r registry) pypiSettled(ctx context.Context, pkg string, cutoff time.Time) (string, error) {
	var doc struct {
		Releases map[string][]struct {
			Uploaded string `json:"upload_time_iso_8601"`
		} `json:"releases"`
	}
	if err := r.getJSON(ctx, r.pypi+"/pypi/"+pkg+"/json", &doc); err != nil {
		return "", err
	}
	var ok []string
	for v, files := range doc.Releases {
		if !stable.MatchString(v) || len(files) == 0 {
			continue
		}
		settled := true
		for _, f := range files {
			t, err := time.Parse(time.RFC3339, f.Uploaded)
			if err != nil || t.After(cutoff) {
				settled = false
				break
			}
		}
		if settled {
			ok = append(ok, v)
		}
	}
	if len(ok) == 0 {
		return "", fmt.Errorf("no release of %s on PyPI before %s", pkg, cutoff.Format(time.DateOnly))
	}
	return newest(ok), nil
}

// settled works out the pins deploy/bump-image.sh would: the newest
// releases that have been out since cutoff, but never older than floor
// (the repository's own pins), and the two Playwrights on the same minor
// version, which is what makes them share a browser build.
func (r registry) settled(ctx context.Context, cutoff time.Time, floor Pins) (Pins, error) {
	claude, err := r.npmSettled(ctx, "@anthropic-ai/claude-code", cutoff, "")
	if err != nil {
		return nil, err
	}
	pi, err := r.npmSettled(ctx, "@earendil-works/pi-coding-agent", cutoff, "")
	if err != nil {
		return nil, err
	}
	python, err := r.pypiSettled(ctx, "playwright", cutoff)
	if err != nil {
		return nil, err
	}
	node, err := r.npmSettled(ctx, "playwright", cutoff, python[:strings.LastIndex(python, ".")])
	if err != nil {
		return nil, err
	}
	out := Pins{pinClaude: claude, pinPi: pi, pinNode: node, pinPython: python}
	for k, v := range floor {
		if slices.Contains(packagePins, k) && later(v, out[k]) {
			out[k] = v
		}
	}
	// A floor ahead of one Playwright but not the other would split them.
	if minorOf(out[pinNode]) != minorOf(out[pinPython]) {
		out[pinNode], out[pinPython] = floor[pinNode], floor[pinPython]
	}
	out[pinReleased] = cutoff.UTC().Format("2006-01-02T15:04:05.000Z")
	return out, nil
}

func minorOf(v string) string {
	if i := strings.LastIndex(v, "."); i > 0 {
		return v[:i]
	}
	return v
}

// Change is one pin moving.
type Change struct {
	Name, From, To string
}

// changes lists the package pins that differ between two sets.
func changes(from, to Pins) []Change {
	var out []Change
	for _, k := range packagePins {
		if from[k] != to[k] {
			out = append(out, Change{Name: pinNames[k], From: from[k], To: to[k]})
		}
	}
	return out
}

func describe(cs []Change) string {
	var s []string
	for _, c := range cs {
		if c.From == "" {
			s = append(s, c.Name+" "+c.To)
		} else {
			s = append(s, c.Name+" "+c.From+" → "+c.To)
		}
	}
	return strings.Join(s, ", ")
}

func samePackages(a, b Pins) bool { return len(changes(a, b)) == 0 }
