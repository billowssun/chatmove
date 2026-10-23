package core

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

var errNotFound = errors.New("not found")

// DAV is a minimal WebDAV client: GET, PUT and MKCOL are all we need, since
// the remote file list lives in manifest.json rather than in PROPFIND results.
type DAV struct {
	base       string
	user, pass string
	hc         *http.Client
	made       map[string]bool
}

func newDAV(cfg *Config) (*DAV, error) {
	u, err := url.Parse(cfg.WebDAVURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid webdav_url %q", cfg.WebDAVURL)
	}
	return &DAV{
		base: strings.TrimRight(cfg.WebDAVURL, "/"),
		user: cfg.Username,
		pass: cfg.Password,
		hc:   &http.Client{Timeout: 30 * time.Minute},
		made: map[string]bool{},
	}, nil
}

func (d *DAV) url(p string) string {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return d.base + "/" + strings.Join(segs, "/")
}

func (d *DAV) do(method, p string, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequest(method, d.url(p), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	if d.user != "" || d.pass != "" {
		req.SetBasicAuth(d.user, d.pass)
	}
	return d.hc.Do(req)
}

func statusErr(method, p string, resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	return fmt.Errorf("%s %s: %s %s", method, p, resp.Status, strings.TrimSpace(string(msg)))
}

func (d *DAV) Get(p string) (io.ReadCloser, error) {
	resp, err := d.do("GET", p, nil, 0)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, statusErr("GET", p, resp)
	}
	return resp.Body, nil
}

func (d *DAV) Put(p string, r io.Reader, size int64) error {
	if err := d.MkdirAll(path.Dir(p)); err != nil {
		return err
	}
	resp, err := d.do("PUT", p, r, size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusErr("PUT", p, resp)
	}
	return nil
}

func (d *DAV) MkdirAll(dir string) error {
	dir = strings.Trim(dir, "/")
	if dir == "" || dir == "." || d.made[dir] {
		return nil
	}
	if err := d.MkdirAll(path.Dir(dir)); err != nil {
		return err
	}
	resp, err := d.do("MKCOL", dir, nil, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 201 created; 405 already exists; some servers answer 301/409 for existing.
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusMethodNotAllowed, http.StatusMovedPermanently, http.StatusOK:
	default:
		return statusErr("MKCOL", dir, resp)
	}
	d.made[dir] = true
	return nil
}
