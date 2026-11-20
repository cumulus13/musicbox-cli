// Full-screen terminal player: browse/search, queue, live synced lyrics, playback keys.
// Audio is played by mpv (controlled over its JSON IPC); the screen is drawn with plain ANSI codes.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

const (
	reset = "\x1b[0m"
	bold  = "\x1b[1m"
	dim   = "\x1b[2m"
	rev   = "\x1b[7m"
	acc   = "\x1b[38;5;79m"
)

// ---------------------------------------------------------------- mpv over IPC

type mpvEvent struct {
	name string
	data any
}

type mpv struct {
	cmd  *exec.Cmd
	conn net.Conn
	mu   sync.Mutex
	path string
}

func startMpv(c *client, events chan<- mpvEvent) (*mpv, error) {
	path := ipcPath()
	args := append([]string{"--no-terminal", "--no-video", "--force-window=no", "--idle=yes",
		"--input-ipc-server=" + path, "--http-header-fields=Authorization: Bearer " + c.cfg.Token}, c.mpvTLSArgs()...)
	cmd := exec.Command("mpv", args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var conn net.Conn
	var err error
	for i := 0; i < 50; i++ {
		if conn, err = dialIPC(path); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("cannot connect to mpv: %w", err)
	}
	m := &mpv{cmd: cmd, conn: conn, path: path}
	for i, p := range []string{"time-pos", "duration", "pause", "playlist-pos", "volume"} {
		m.send("observe_property", i+1, p)
	}
	go m.readLoop(events)
	return m, nil
}

func (m *mpv) send(args ...any) {
	b, err := json.Marshal(map[string]any{"command": args})
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, _ = m.conn.Write(append(b, '\n'))
}

func (m *mpv) readLoop(events chan<- mpvEvent) {
	sc := bufio.NewScanner(m.conn)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var msg map[string]any
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		switch msg["event"] {
		case "property-change":
			name, _ := msg["name"].(string)
			events <- mpvEvent{name: name, data: msg["data"]}
		case "shutdown":
			events <- mpvEvent{name: "shutdown"}
		}
	}
	events <- mpvEvent{name: "closed"}
}

func (m *mpv) quit() {
	m.send("quit")
	_ = m.conn.Close()
	done := make(chan struct{})
	go func() {
		_ = m.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		_ = m.cmd.Process.Kill()
	}
	_ = os.Remove(m.path)
}

// ---------------------------------------------------------------- UI state

type viewID int

const (
	viewBrowse viewID = iota
	viewQueue
	viewLyrics
)

type item struct {
	label  string
	kind   string // header | mix | artist | album | track
	target string
	t      track
}

type lyricLine struct {
	T    float64 `json:"t"`
	Text string  `json:"text"`
}

type lyricsData struct {
	id      int
	loading bool
	loaded  bool
	status  string
	matched string
	plain   string
	synced  []lyricLine
}

type ui struct {
	c       *client
	m       *mpv
	view    viewID
	items   []item
	sel     int
	queue   []track
	qsel    int
	cur     int
	pos     float64
	dur     float64
	paused  bool
	repeat  int // 0 off, 1 all, 2 one
	volume  float64
	lyrics  lyricsData
	lscroll int
	lead    float64
	status  string
	input   bool
	buf     string
	page    int
	posts   chan func(*ui)
}

func menuItems() []item {
	return []item{
		{label: "Flow  (your favorites + discoveries)", kind: "mix", target: "flow"},
		{label: "Random mix", kind: "mix", target: "random"},
		{label: "Favorites", kind: "mix", target: "favorites"},
		{label: "Press / to search artists, albums and tracks", kind: "header"},
	}
}

func cmdTui(c *client) error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return errors.New("tui needs an interactive terminal")
	}
	if _, err := exec.LookPath("mpv"); err != nil {
		return errors.New("the TUI needs mpv: install it and make sure it is in PATH")
	}
	events := make(chan mpvEvent, 256)
	m, err := startMpv(c, events)
	if err != nil {
		return err
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		m.quit()
		return err
	}
	enableVT()
	fmt.Print("\x1b[?1049h\x1b[?25l")
	defer func() {
		fmt.Print("\x1b[?25h\x1b[?1049l")
		_ = term.Restore(fd, old)
		m.quit()
	}()

	u := &ui{c: c, m: m, cur: -1, volume: 100, items: menuItems(), lead: 0.3, page: 10,
		posts: make(chan func(*ui), 64)}
	keys := make(chan string, 64)
	go readKeys(keys)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	u.render()
	for {
		select {
		case k := <-keys:
			if u.onKey(k) {
				return nil
			}
		case e := <-events:
			if u.onMpv(e) {
				return nil
			}
			if e.name == "time-pos" {
				continue // redrawn by the ticker
			}
		case f := <-u.posts:
			f(u)
		case <-tick.C:
		}
		u.render()
	}
}

func (u *ui) post(f func(*ui)) { u.posts <- f }

// ---------------------------------------------------------------- input

func readKeys(out chan<- string) {
	buf := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			out <- "ctrl-c"
			return
		}
		for _, k := range parseKeys(buf[:n]) {
			out <- k
		}
	}
}

func parseKeys(b []byte) []string {
	var keys []string
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b && i+2 < len(b) && (b[i+1] == '[' || b[i+1] == 'O'):
			j := i + 2
			for j < len(b) && b[j] >= 0x30 && b[j] <= 0x3f {
				j++
			}
			if j >= len(b) {
				i = len(b)
				continue
			}
			params := string(b[i+2 : j])
			final := b[j]
			i = j + 1
			switch final {
			case 'A':
				keys = append(keys, "up")
			case 'B':
				keys = append(keys, "down")
			case 'C':
				keys = append(keys, "right")
			case 'D':
				keys = append(keys, "left")
			case 'H':
				keys = append(keys, "home")
			case 'F':
				keys = append(keys, "end")
			case '~':
				switch strings.SplitN(params, ";", 2)[0] {
				case "5":
					keys = append(keys, "pgup")
				case "6":
					keys = append(keys, "pgdn")
				case "1", "7":
					keys = append(keys, "home")
				case "4", "8":
					keys = append(keys, "end")
				case "3":
					keys = append(keys, "delete")
				}
			}
		case c == 0x1b:
			keys = append(keys, "esc")
			i++
		case c == 3:
			keys = append(keys, "ctrl-c")
			i++
		case c == '\r' || c == '\n':
			keys = append(keys, "enter")
			i++
		case c == '\t':
			keys = append(keys, "tab")
			i++
		case c == ' ':
			keys = append(keys, "space")
			i++
		case c == 127 || c == 8:
			keys = append(keys, "backspace")
			i++
		default:
			r, size := utf8.DecodeRune(b[i:])
			if r != utf8.RuneError || size > 1 {
				keys = append(keys, string(r))
			}
			i += size
		}
	}
	return keys
}

func move(sel *int, n int, k string, page int) {
	switch k {
	case "up", "k":
		*sel--
	case "down", "j":
		*sel++
	case "pgup":
		*sel -= page
	case "pgdn":
		*sel += page
	case "home":
		*sel = 0
	case "end":
		*sel = n - 1
	}
	if *sel >= n {
		*sel = n - 1
	}
	if *sel < 0 {
		*sel = 0
	}
}

// onKey returns true when the user asked to quit.
func (u *ui) onKey(k string) bool {
	if u.input {
		u.onInputKey(k)
		return false
	}
	u.status = ""
	switch k {
	case "q", "ctrl-c":
		return true
	case "1":
		u.view = viewBrowse
	case "2":
		u.view = viewQueue
	case "3":
		u.view = viewLyrics
	case "tab":
		u.view = (u.view + 1) % 3
	case "space":
		u.m.send("cycle", "pause")
	case "n":
		u.m.send("playlist-next")
	case "p":
		u.m.send("playlist-prev")
	case "r":
		u.cycleRepeat()
	case "b":
		u.m.send("seek", 0, "absolute") // restart the current track
	case "left":
		u.m.send("seek", -10, "relative")
	case "right":
		u.m.send("seek", 10, "relative")
	case "+", "=":
		u.m.send("add", "volume", 5)
	case "-":
		u.m.send("add", "volume", -5)
	case "/":
		u.input, u.buf, u.view = true, "", viewBrowse
	default:
		u.onListKey(k)
	}
	return false
}

func (u *ui) onInputKey(k string) {
	switch k {
	case "esc", "ctrl-c":
		u.input = false
	case "enter":
		q := strings.TrimSpace(u.buf)
		u.input = false
		if q != "" {
			u.status = "Searching " + q + " ..."
			go u.search(q)
		}
	case "backspace":
		if r := []rune(u.buf); len(r) > 0 {
			u.buf = string(r[:len(r)-1])
		}
	case "space":
		u.buf += " "
	default:
		if utf8.RuneCountInString(k) == 1 && k >= " " {
			u.buf += k
		}
	}
}

func (u *ui) onListKey(k string) {
	switch u.view {
	case viewBrowse:
		move(&u.sel, len(u.items), k, u.page)
		switch k {
		case "enter":
			u.activate()
		case "a":
			u.addSelected()
		case "backspace":
			u.items, u.sel = menuItems(), 0
		}
	case viewQueue:
		move(&u.qsel, len(u.queue), k, u.page)
		switch k {
		case "enter":
			if u.qsel < len(u.queue) {
				u.m.send("playlist-play-index", u.qsel)
			}
		case "d", "x", "delete":
			u.removeQueued(u.qsel)
		case "c":
			u.queue, u.cur, u.qsel = nil, -1, 0
			u.lyrics = lyricsData{}
			u.m.send("stop")
		}
	case viewLyrics:
		switch k {
		case "up", "k":
			if u.lscroll > 0 {
				u.lscroll--
			}
		case "down", "j":
			u.lscroll++
		case "pgup":
			u.lscroll -= u.page
			if u.lscroll < 0 {
				u.lscroll = 0
			}
		case "pgdn":
			u.lscroll += u.page
		}
	}
}

// ---------------------------------------------------------------- actions

func (u *ui) activate() {
	if u.sel < 0 || u.sel >= len(u.items) {
		return
	}
	it := u.items[u.sel]
	switch it.kind {
	case "mix", "artist", "album":
		u.status = "Loading ..."
		go u.fetchAndPlay(it.target)
	case "track":
		var list []track
		start := 0
		for i, x := range u.items {
			if x.kind != "track" {
				continue
			}
			if i == u.sel {
				start = len(list)
			}
			list = append(list, x.t)
		}
		u.playList(list, start)
	}
}

func (u *ui) fetchAndPlay(target string) {
	list, err := resolve(u.c, target)
	u.post(func(u *ui) {
		switch {
		case err != nil:
			u.status = "error: " + err.Error()
		case len(list) == 0:
			u.status = "nothing to play"
		default:
			u.playList(list, 0)
		}
	})
}

// playList replaces the queue with list[start:] and starts playing.
func (u *ui) playList(list []track, start int) {
	if start < 0 || start >= len(list) {
		start = 0
	}
	list = list[start:]
	u.queue, u.cur, u.qsel = list, 0, 0
	u.lyrics, u.lscroll = lyricsData{}, 0
	for i, t := range list {
		mode := "append"
		if i == 0 {
			mode = "replace"
		}
		u.m.send("loadfile", u.c.cfg.Server+t.URL, mode)
	}
	u.m.send("set_property", "pause", false)
	u.loadLyrics()
	u.status = fmt.Sprintf("Playing %d tracks", len(list))
}

func (u *ui) enqueue(t track) {
	u.enqueueMany([]track{t})
	u.status = "Added: " + t.Title
}

// enqueueMany appends tracks to the queue; the first one starts playback if nothing is playing.
func (u *ui) enqueueMany(list []track) {
	for i, t := range list {
		mode := "append"
		if i == 0 {
			mode = "append-play"
		}
		u.queue = append(u.queue, t)
		u.m.send("loadfile", u.c.cfg.Server+t.URL, mode)
	}
}

// addSelected appends the selected track, or every track of the selected artist, album or mix.
func (u *ui) addSelected() {
	if u.sel < 0 || u.sel >= len(u.items) {
		return
	}
	it := u.items[u.sel]
	switch it.kind {
	case "track":
		u.enqueue(it.t)
	case "mix", "artist", "album":
		u.status = "Adding ..."
		go u.fetchAndAdd(it.target)
	}
}

func (u *ui) fetchAndAdd(target string) {
	list, err := resolve(u.c, target)
	u.post(func(u *ui) {
		switch {
		case err != nil:
			u.status = "error: " + err.Error()
		case len(list) == 0:
			u.status = "nothing to add"
		default:
			u.enqueueMany(list)
			u.status = fmt.Sprintf("Added %d tracks to the queue", len(list))
		}
	})
}

func (u *ui) cycleRepeat() {
	u.repeat = (u.repeat + 1) % 3
	file, list := "no", "no"
	switch u.repeat {
	case 1:
		list = "inf"
	case 2:
		file = "inf"
	}
	u.m.send("set_property", "loop-file", file)
	u.m.send("set_property", "loop-playlist", list)
	u.status = "Repeat: " + []string{"off", "all", "one"}[u.repeat]
}

func (u *ui) removeQueued(i int) {
	if i < 0 || i >= len(u.queue) {
		return
	}
	title := u.queue[i].Title
	u.m.send("playlist-remove", i)
	u.queue = append(u.queue[:i], u.queue[i+1:]...)
	if i < u.cur {
		u.cur--
	}
	u.qsel = clamp(u.qsel, 0, len(u.queue)-1)
	u.status = "Removed: " + title
}

func (u *ui) search(q string) {
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
	err := u.c.do("GET", "/api/v1/search?q="+url.QueryEscape(q), nil, &res)
	u.post(func(u *ui) {
		if err != nil {
			u.status = "error: " + err.Error()
			return
		}
		var items []item
		for _, a := range res.Artists {
			items = append(items, item{label: a.Name, kind: "artist", target: fmt.Sprintf("artist:%d", a.ID)})
		}
		for _, a := range res.Albums {
			items = append(items, item{label: a.Artist + " - " + a.Title, kind: "album", target: fmt.Sprintf("album:%d", a.ID)})
		}
		for _, t := range res.Tracks {
			items = append(items, item{label: fmt.Sprintf("%s - %s (%s)", t.Artist, t.Title, fmtDur(t.Duration)), kind: "track", t: t})
		}
		if len(items) == 0 {
			items = []item{{label: "No results for \"" + q + "\"", kind: "header"}}
		}
		u.items, u.sel, u.view = items, 0, viewBrowse
		u.status = fmt.Sprintf("%d results  (Enter plays, a adds a track, Backspace = menu)", len(items))
	})
}

func (u *ui) loadLyrics() {
	if u.cur < 0 || u.cur >= len(u.queue) {
		return
	}
	id := u.queue[u.cur].ID
	if u.lyrics.id == id && (u.lyrics.loaded || u.lyrics.loading) {
		return
	}
	u.lyrics = lyricsData{id: id, loading: true}
	u.lscroll = 0
	c := u.c
	go func() {
		var res struct {
			Status  string      `json:"status"`
			Matched string      `json:"matched"`
			Plain   string      `json:"plain"`
			Synced  []lyricLine `json:"synced"`
		}
		err := c.do("GET", fmt.Sprintf("/api/v1/tracks/%d/lyrics", id), nil, &res)
		u.post(func(u *ui) {
			if u.lyrics.id != id {
				return
			}
			if err != nil {
				u.lyrics = lyricsData{id: id, loaded: true, status: "error: " + err.Error()}
				return
			}
			u.lyrics = lyricsData{id: id, loaded: true, status: res.Status, matched: res.Matched,
				plain: res.Plain, synced: res.Synced}
		})
	}()
}

// onMpv returns true when mpv went away.
func (u *ui) onMpv(e mpvEvent) bool {
	switch e.name {
	case "time-pos":
		f, _ := e.data.(float64)
		u.pos = f
	case "duration":
		f, _ := e.data.(float64)
		u.dur = f
	case "pause":
		if b, ok := e.data.(bool); ok {
			u.paused = b
		}
	case "volume":
		if f, ok := e.data.(float64); ok {
			u.volume = f
		}
	case "playlist-pos":
		if f, ok := e.data.(float64); ok {
			u.cur = int(f)
			u.loadLyrics()
		} else {
			u.cur = -1
		}
	case "shutdown", "closed":
		return true
	}
	return false
}

// ---------------------------------------------------------------- drawing

func clamp(v, lo, hi int) int {
	if hi < lo {
		hi = lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func trunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

func padRight(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

func center(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		return s
	}
	return strings.Repeat(" ", (w-n)/2) + s
}

func icon(kind string) string {
	switch kind {
	case "mix":
		return "▶ "
	case "artist":
		return "♪ "
	case "album":
		return "◉ "
	case "track":
		return "  "
	}
	return ""
}

func (u *ui) render() {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w < 30 || h < 8 {
		w, h = 80, 24
	}
	n := h - 4
	u.page = n - 1
	lines := []string{u.headerLine()}
	lines = append(lines, u.bodyLines(w, n)...)
	lines = append(lines, u.nowLine(w), u.barLine(w), u.helpLine(w))
	var sb strings.Builder
	sb.WriteString("\x1b[H")
	for i, l := range lines {
		sb.WriteString(l)
		sb.WriteString("\x1b[K")
		if i < len(lines)-1 {
			sb.WriteString("\r\n")
		}
	}
	sb.WriteString("\x1b[J")
	_, _ = os.Stdout.WriteString(sb.String())
}

func (u *ui) headerLine() string {
	var sb strings.Builder
	sb.WriteString(" " + bold + acc + "♪ MusicBox" + reset + "   ")
	for i, name := range []string{"1 Browse", "2 Queue", "3 Lyrics"} {
		if viewID(i) == u.view {
			sb.WriteString(rev + " " + name + " " + reset)
		} else {
			sb.WriteString(dim + " " + name + " " + reset)
		}
		sb.WriteString(" ")
	}
	return sb.String()
}

func (u *ui) bodyLines(w, n int) []string {
	var lines []string
	switch u.view {
	case viewQueue:
		lines = u.queueLines(w, n)
	case viewLyrics:
		lines = u.lyricLines(w, n)
	default:
		lines = u.browseLines(w, n)
	}
	for len(lines) < n {
		lines = append(lines, "")
	}
	return lines[:n]
}

func (u *ui) browseLines(w, n int) []string {
	var lines []string
	start := clamp(u.sel-n/2, 0, len(u.items)-n)
	for i := start; i < len(u.items) && len(lines) < n; i++ {
		it := u.items[i]
		text := trunc(" "+icon(it.kind)+it.label, w-1)
		switch {
		case i == u.sel:
			lines = append(lines, rev+padRight(text, w)+reset)
		case it.kind == "header":
			lines = append(lines, dim+text+reset)
		default:
			lines = append(lines, text)
		}
	}
	return lines
}

func (u *ui) queueLines(w, n int) []string {
	if len(u.queue) == 0 {
		return []string{dim + " The queue is empty. Play something from Browse." + reset}
	}
	var lines []string
	start := clamp(u.qsel-n/2, 0, len(u.queue)-n)
	for i := start; i < len(u.queue) && len(lines) < n; i++ {
		t := u.queue[i]
		mark := "  "
		if i == u.cur {
			mark = "▶ "
		}
		text := trunc(fmt.Sprintf(" %s%3d. %s - %s", mark, i+1, t.Artist, t.Title), w-1)
		if i == u.qsel {
			lines = append(lines, rev+padRight(text, w)+reset)
		} else if i == u.cur {
			lines = append(lines, acc+text+reset)
		} else {
			lines = append(lines, text)
		}
	}
	return lines
}

func (u *ui) lyricLines(w, n int) []string {
	if u.cur < 0 || u.cur >= len(u.queue) {
		return []string{dim + " Nothing playing." + reset}
	}
	l := u.lyrics
	head := dim + trunc(" "+l.status+"  "+l.matched, w-1) + reset
	if !l.loaded {
		return []string{head, " Loading lyrics ..."}
	}
	room := n - 1
	lines := []string{head}
	switch {
	case len(l.synced) > 0:
		idx := -1
		for i, ln := range l.synced {
			if ln.T <= u.pos+u.lead {
				idx = i
			}
		}
		start := clamp(idx-room/2, 0, len(l.synced)-room)
		for i := start; i < len(l.synced) && i < start+room; i++ {
			text := l.synced[i].Text
			if text == "" {
				text = "♪"
			}
			text = center(trunc(text, w-2), w)
			if i == idx {
				text = bold + acc + text + reset
			} else {
				text = dim + text + reset
			}
			lines = append(lines, text)
		}
	case l.plain != "":
		all := strings.Split(l.plain, "\n")
		start := clamp(u.lscroll, 0, len(all)-room)
		u.lscroll = start
		for i := start; i < len(all) && i < start+room; i++ {
			lines = append(lines, " "+trunc(all[i], w-2))
		}
	default:
		lines = append(lines, " No lyrics for this track.")
	}
	return lines
}

func (u *ui) nowLine(w int) string {
	if u.cur < 0 || u.cur >= len(u.queue) {
		return dim + " Nothing playing" + reset
	}
	t := u.queue[u.cur]
	ic := "▶"
	if u.paused {
		ic = "⏸"
	}
	return " " + acc + bold + ic + " " + trunc(t.Artist+" — "+t.Title, w-4) + reset
}

func (u *ui) barLine(w int) string {
	right := fmt.Sprintf(" %s / %s  vol %d%%%s", fmtDur(u.pos), fmtDur(u.dur), int(u.volume), []string{"", "  repeat: all", "  repeat: one"}[u.repeat])
	barw := w - utf8.RuneCountInString(right) - 2
	if barw < 5 {
		barw = 5
	}
	filled := 0
	if u.dur > 0 {
		filled = clamp(int(float64(barw)*u.pos/u.dur), 0, barw)
	}
	return " " + acc + strings.Repeat("█", filled) + reset + dim + strings.Repeat("░", barw-filled) + reset + right
}

func (u *ui) helpLine(w int) string {
	if u.input {
		return " Search: " + u.buf + "▏" + dim + "   Enter = go, Esc = cancel" + reset
	}
	if u.status != "" {
		return " " + trunc(u.status, w-2)
	}
	keys := "/ search · Enter play · a add (track/artist/album) · r repeat · b restart · space pause · n/p · ←/→ seek · +/- vol · tab view · q quit"
	switch u.view {
	case viewQueue:
		keys = "Enter play · d/x/Del remove · c clear · r repeat · b restart · space pause · n/p · ←/→ seek · +/- vol · tab view · q quit"
	case viewLyrics:
		keys = "↑/↓ scroll · r repeat · b restart · space pause · n/p · ←/→ seek · +/- vol · tab view · q quit"
	}
	return dim + " " + trunc(keys, w-2) + reset
}
