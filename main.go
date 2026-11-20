// MusicBox command-line client (standard library only).
// Search, stream-play through mpv/ffplay, lyrics, and MPD speaker control (admin accounts).
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type config struct {
	Server   string `json:"server"`
	Token    string `json:"token"`
	Email    string `json:"email"`
	Insecure bool   `json:"insecure,omitempty"` // skip TLS verification (local/test servers only)
	CACert   string `json:"cacert,omitempty"`   // PEM file of a private CA
}

type track struct {
	ID       int     `json:"id"`
	Title    string  `json:"title"`
	Artist   string  `json:"artist"`
	Album    string  `json:"album"`
	URL      string  `json:"url"`
	Duration float64 `json:"duration"`
}

type client struct {
	cfg  config
	http *http.Client
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "musicbox", "config.json"), nil
}

func loadConfig() (config, error) {
	var c config
	p, err := configPath()
	if err != nil {
		return c, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return c, errors.New("not logged in (run: musicbox login)")
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	return c, nil
}

func saveConfig(c config) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

func (c *client) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(c.cfg.Server, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "x509") || strings.Contains(err.Error(), "tls:") {
			return fmt.Errorf("%w hint: for a local or self-signed server run: musicbox login -insecure ... (or -cacert FILE)", err)
		}
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errors.New("unauthorized: wrong credentials, or run: musicbox login")
	case resp.StatusCode == http.StatusForbidden:
		return errors.New("forbidden: this command needs an admin account")
	case resp.StatusCode >= 300:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func envTrue(name string) bool {
	switch strings.ToLower(os.Getenv(name)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// normalizeServer trims the URL and assumes https:// when no scheme is given.
func normalizeServer(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s != "" && !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return s
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if via[0].URL.Scheme == "http" && req.URL.Scheme == "https" {
		return fmt.Errorf("the server redirects http:// to https://: use -server https://%s (add -insecure for a self-signed certificate)", req.URL.Host)
	}
	return nil
}

// newClient builds the HTTP client. cfg.Insecure disables certificate verification (explicit opt-in,
// meant for local/test servers); cfg.CACert trusts an extra CA file instead.
func newClient(cfg config, timeout time.Duration) (*client, error) {
	var tr *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	} else {
		tr = &http.Transport{}
	}
	tlsCfg := &tls.Config{}
	switch {
	case cfg.Insecure:
		tlsCfg.InsecureSkipVerify = true
	case cfg.CACert != "":
		pem, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("cannot read CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("no certificates found in " + cfg.CACert)
		}
		tlsCfg.RootCAs = pool
	}
	tr.TLSClientConfig = tlsCfg
	return &client{cfg: cfg, http: &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: checkRedirect}}, nil
}

// TLS options for the external players, matching the client's settings.
func (c *client) mpvTLSArgs() []string {
	switch {
	case c.cfg.Insecure:
		return []string{"--tls-verify=no"}
	case c.cfg.CACert != "":
		return []string{"--tls-verify=yes", "--tls-ca-file=" + c.cfg.CACert}
	}
	return nil
}

func (c *client) ffplayTLSArgs() []string {
	switch {
	case c.cfg.Insecure:
		return []string{"-tls_verify", "0"}
	case c.cfg.CACert != "":
		return []string{"-ca_file", c.cfg.CACert}
	}
	return nil
}

func prompt(r *bufio.Reader, label string) string {
	fmt.Print(label)
	s, _ := r.ReadString('\n')
	return strings.TrimSpace(s)
}

func fmtDur(sec float64) string {
	s := int(sec)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func oneLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	server := fs.String("server", os.Getenv("MUSICBOX_URL"), "server URL, e.g. https://music.example.com")
	email := fs.String("email", os.Getenv("MUSICBOX_EMAIL"), "account email")
	insecure := fs.Bool("insecure", envTrue("MUSICBOX_INSECURE"), "skip TLS certificate verification (local/test servers only)")
	cacert := fs.String("cacert", os.Getenv("MUSICBOX_CACERT"), "PEM file of the CA that signed the server certificate")
	fs.Parse(args)
	in := bufio.NewReader(os.Stdin)
	if *server == "" {
		*server = prompt(in, "Server URL: ")
	}
	if *email == "" {
		*email = prompt(in, "Email: ")
	}
	pass := os.Getenv("MUSICBOX_PASSWORD")
	if pass == "" {
		pass = prompt(in, "Password (visible; or set MUSICBOX_PASSWORD): ")
	}
	if *cacert != "" {
		if abs, err := filepath.Abs(*cacert); err == nil {
			*cacert = abs
		}
	}
	c, err := newClient(config{Server: normalizeServer(*server), Insecure: *insecure, CACert: *cacert}, 20*time.Second)
	if err != nil {
		return err
	}
	if c.cfg.Insecure {
		fmt.Fprintln(os.Stderr, "WARNING: TLS certificate verification is disabled (use only for local/test servers).")
	}
	var out struct {
		Token string `json:"token"`
		Email string `json:"email"`
		Admin bool   `json:"admin"`
	}
	if err := c.do("POST", "/api/v1/token", map[string]string{"email": *email, "password": pass}, &out); err != nil {
		return err
	}
	c.cfg.Token, c.cfg.Email = out.Token, out.Email
	if err := saveConfig(c.cfg); err != nil {
		return err
	}
	role := ""
	if out.Admin {
		role = " (admin)"
	}
	fmt.Printf("Logged in as %s%s\n", out.Email, role)
	return nil
}

func cmdLogout(c *client) error {
	_ = c.do("DELETE", "/api/v1/token", nil, nil)
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("Logged out.")
	return nil
}

func cmdSearch(c *client, args []string) error {
	q := strings.Join(args, " ")
	if q == "" {
		return errors.New("usage: musicbox search <text>")
	}
	var res struct {
		Artists []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"artists"`
		Albums []struct {
			ID     int    `json:"id"`
			Title  string `json:"title"`
			Artist string `json:"artist"`
		} `json:"albums"`
		Tracks []track `json:"tracks"`
	}
	if err := c.do("GET", "/api/v1/search?q="+url.QueryEscape(q), nil, &res); err != nil {
		return err
	}
	for _, a := range res.Artists {
		fmt.Printf("artist:%-7d %s\n", a.ID, a.Name)
	}
	for _, a := range res.Albums {
		fmt.Printf("album:%-8d %s - %s\n", a.ID, a.Artist, a.Title)
	}
	for _, t := range res.Tracks {
		fmt.Printf("track:%-8d %s - %s (%s)\n", t.ID, t.Artist, t.Title, fmtDur(t.Duration))
	}
	return nil
}

// resolve turns a target (flow, random, favorites, album:ID, artist:ID, track:ID or free text) into tracks.
func resolve(c *client, target string) ([]track, error) {
	var list []track
	switch {
	case target == "flow" || target == "random" || target == "favorites":
		err := c.do("GET", "/api/v1/mix/"+target, nil, &list)
		return list, err
	case strings.HasPrefix(target, "album:") || strings.HasPrefix(target, "artist:") || strings.HasPrefix(target, "track:"):
		kind, id, _ := strings.Cut(target, ":")
		err := c.do("GET", "/api/v1/mix/"+kind+"/"+url.PathEscape(id), nil, &list)
		return list, err
	default:
		var res struct {
			Tracks []track `json:"tracks"`
		}
		err := c.do("GET", "/api/v1/search?q="+url.QueryEscape(target), nil, &res)
		return res.Tracks, err
	}
}

func cmdPlay(c *client, args []string) error {
	fs := flag.NewFlagSet("play", flag.ExitOnError)
	player := fs.String("player", "", "mpv or ffplay (default: first one found)")
	shuffle := fs.Bool("shuffle", false, "shuffle the list")
	fs.Parse(args)
	target := strings.Join(fs.Args(), " ")
	if target == "" {
		target = "flow"
	}
	list, err := resolve(c, target)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return errors.New("nothing to play")
	}
	if *shuffle {
		rand.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
	}
	for i, t := range list {
		fmt.Printf("%3d. %s - %s (%s)\n", i+1, t.Artist, t.Title, fmtDur(t.Duration))
	}
	name := *player
	if name == "" {
		for _, n := range []string{"mpv", "ffplay"} {
			if _, err := exec.LookPath(n); err == nil {
				name = n
				break
			}
		}
	}
	switch name {
	case "mpv":
		return playMpv(c, list)
	case "ffplay":
		return playFfplay(c, list)
	case "":
		return errors.New("no player found: install mpv (recommended) or ffplay")
	}
	return fmt.Errorf("unsupported player %q (use mpv or ffplay)", name)
}

// mpv gets the whole queue as an M3U playlist; its own keys work (space, < >, q).
func playMpv(c *client, list []track) error {
	f, err := os.CreateTemp("", "musicbox-*.m3u")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	fmt.Fprintln(f, "#EXTM3U")
	for _, t := range list {
		fmt.Fprintf(f, "#EXTINF:%d,%s\n%s%s\n", int(t.Duration), oneLine(t.Artist+" - "+t.Title), c.cfg.Server, t.URL)
	}
	f.Close()
	args := append([]string{"--no-video", "--force-window=no",
		"--http-header-fields=Authorization: Bearer " + c.cfg.Token, "--playlist=" + f.Name()}, c.mpvTLSArgs()...)
	cmd := exec.Command("mpv", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// ffplay plays one track at a time: q skips to the next track, Ctrl-C stops everything.
func playFfplay(c *client, list []track) error {
	for i, t := range list {
		fmt.Printf("[%d/%d] %s - %s\n", i+1, len(list), t.Artist, t.Title)
		args := append([]string{"-nodisp", "-autoexit", "-loglevel", "error", "-headers", "Authorization: Bearer " + c.cfg.Token }, c.ffplayTLSArgs()...)
		cmd := exec.Command("ffplay", append(args, c.cfg.Server+t.URL)...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "player:", err)
		}
	}
	return nil
}

func cmdLyrics(c *client, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: musicbox lyrics <track id>   (ids are shown by: musicbox search)")
	}
	var res struct {
		Status  string `json:"status"`
		Matched string `json:"matched"`
		Plain   string `json:"plain"`
		Synced  []struct {
			T    float64 `json:"t"`
			Text string  `json:"text"`
		} `json:"synced"`
	}
	if err := c.do("GET", "/api/v1/tracks/"+url.PathEscape(args[0])+"/lyrics", nil, &res); err != nil {
		return err
	}
	fmt.Printf("status: %s  %s\n\n", res.Status, res.Matched)
	if len(res.Synced) > 0 {
		for _, l := range res.Synced {
			fmt.Printf("[%s] %s\n", fmtDur(l.T), l.Text)
		}
		return nil
	}
	fmt.Println(res.Plain)
	return nil
}

// cmdMpd controls the MPD speakers on the server (admin accounts only).
func cmdMpd(c *client, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: musicbox mpd status|play|pause|stop|next|prev|volume N|seek SEC|shuffle true|repeat true|queue|add TRACK_ID")
	}
	switch args[0] {
	case "status":
		var st map[string]any
		if err := c.do("GET", "/api/v1/player/status", nil, &st); err != nil {
			return err
		}
		fmt.Printf("state: %v  volume: %v  elapsed: %v\n", st["state"], st["volume"], st["elapsed"])
		if song, ok := st["song"].(map[string]any); ok {
			fmt.Printf("now: %v - %v (%v)\n", song["artist"], song["title"], song["album"])
		}
	case "play", "pause", "stop", "next", "prev":
		return c.do("POST", "/api/v1/player/command", map[string]any{"cmd": args[0]}, nil)
	case "volume", "seek", "shuffle", "random", "repeat", "single", "consume":
		if len(args) < 2 {
			return fmt.Errorf("usage: musicbox mpd %s VALUE", args[0])
		}
		return c.do("POST", "/api/v1/player/command", map[string]any{"cmd": args[0], "value": args[1]}, nil)
	case "queue":
		var q []struct {
			Title  string `json:"title"`
			Artist string `json:"artist"`
		}
		if err := c.do("GET", "/api/v1/queue", nil, &q); err != nil {
			return err
		}
		for i, t := range q {
			fmt.Printf("%3d. %s - %s\n", i+1, t.Artist, t.Title)
		}
	case "add":
		if len(args) < 2 {
			return errors.New("usage: musicbox mpd add TRACK_ID")
		}
		return c.do("POST", "/api/v1/queue", map[string]any{"track_id": args[1], "play": true}, nil)
	default:
		return fmt.Errorf("unknown mpd command %q", args[0])
	}
	return nil
}

func usage() {
	fmt.Println(`musicbox - command-line client

  musicbox login [-server URL] [-email EMAIL] [-insecure] [-cacert FILE]
  musicbox logout
  musicbox search <text>
  musicbox play [-player mpv|ffplay] [-shuffle] [flow|random|favorites|album:ID|artist:ID|track:ID|<text>]
  musicbox tui                      full-screen player (needs mpv)
  musicbox lyrics <track id>
  musicbox mpd status|play|pause|stop|next|prev|volume N|seek SEC|queue|add TRACK_ID   (admin)

Environment: MUSICBOX_URL, MUSICBOX_EMAIL, MUSICBOX_PASSWORD, MUSICBOX_INSECURE=1, MUSICBOX_CACERT`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "login":
		err = cmdLogin(args)
	case "help", "-h", "--help":
		usage()
	default:
		cfg, e := loadConfig()
		if e != nil {
			fmt.Fprintln(os.Stderr, "error:", e)
			os.Exit(1)
		}
		if envTrue("MUSICBOX_INSECURE") {
			cfg.Insecure = true
		}
		c, e := newClient(cfg, 30*time.Second)
		if e != nil {
			fmt.Fprintln(os.Stderr, "error:", e)
			os.Exit(1)
		}
		switch cmd {
		case "logout":
			err = cmdLogout(c)
		case "search":
			err = cmdSearch(c, args)
		case "play":
			err = cmdPlay(c, args)
		case "lyrics":
			err = cmdLyrics(c, args)
		case "mpd":
			err = cmdMpd(c, args)
		case "tui", "ui":
			err = cmdTui(c)
		default:
			usage()
			os.Exit(2)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
