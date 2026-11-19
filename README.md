# musicbox CLI

Standard-library Go client for MusicBox. Needs Go 1.21+ to build and, for playing, `mpv` (recommended) or `ffplay`.

Or download a prebuilt binary from the [Releases](https://github.com/cumulus13/musicbox-cli/releases) page (Linux, macOS, Windows)


<p align="center">
  <img src="https://raw.githubusercontent.com/licface15/images/main/musicbox/demo.gif?raw=true" width="98%" alt="Demo">
</p>


```bash
cd cli && go build -o musicbox .          # Windows: go build -o musicbox.exe .
./musicbox login -server https://music.example.com -email you@example.com
./musicbox search "yellow"                 # prints ids: artist:ID album:ID track:ID
./musicbox play flow                       # stream through mpv; keys: space pause, < > prev/next, q quit
./musicbox play album:2209
./musicbox play artist:501 -shuffle
./musicbox play favorites
./musicbox lyrics 12470
./musicbox mpd status                      # admin accounts: control the MPD speakers on the server
./musicbox mpd pause | next | volume 40 | queue | add 12470
```
`login` stores a bearer token in your user config directory (`config.json`, mode 0600). Logging in again from another machine replaces the token (one token per account); `logout` revokes it. The password prompt is visible; set `MUSICBOX_PASSWORD` to avoid typing it.
Playback happens on the machine running the CLI (audio is streamed from `/media/:id`). It is independent from the web player: the browser queue cannot be remote-controlled. Only `mpd ...` commands control a shared player (the server's speakers).

## Full-screen player (`musicbox tui`)
```bash
go mod tidy && go build -o musicbox .     # first time only needs network (fetches x/term, x/sys, go-winio)
./musicbox tui
```
Needs `mpv` in PATH (the TUI controls it over its JSON IPC). Views: **1 Browse** (Flow, Random, Favorites, search results), **2 Queue**, **3 Lyrics** (synced lines follow the playback position, +0.3 s lead).

| Key | Action |
|---|---|
| `/` | search (Enter runs it; results: artists, albums, tracks) |
| `↑ ↓ PgUp PgDn Home End` (or `j k`) | move |
| `Enter` | play the selected item (a track plays the listed tracks from there) |
| `a` | add the selected track to the queue |
| `space` · `n` · `p` | pause/resume · next · previous |
| `← →` | seek -10 s / +10 s |
| `+` `-` | volume |
| `1` `2` `3` / `Tab` | switch view |
| Queue view: `Enter` / `d` / `c` | play / remove / clear |
| `Backspace` (Browse) | back to the menu |
| `q` / `Ctrl-C` | quit (stops mpv) |

Supported on Linux, macOS and WSL (Unix socket to mpv). Native Windows uses a named pipe through `go-winio` and Windows Terminal; it has not been verified. Very wide characters (CJK) can misalign columns.

## Local / test servers without a trusted certificate
```bash
# 1) Plain HTTP, no TLS at all (for example the container's own port, no proxy in front):
./musicbox login -server http://192.168.1.10:30000 -email you@example.com

# 2) HTTPS with a self-signed or private-CA certificate:
./musicbox login -server https://music.local -email you@example.com -insecure       # skips verification (test only)
./musicbox login -server https://music.local -email you@example.com -cacert ca.pem  # safer: trust that CA
```
The choice is saved with the login, so `search`, `play`, `tui`... use it automatically; `MUSICBOX_INSECURE=1` forces it for a single run. `-insecure` also disables certificate checks in mpv/ffplay (`--tls-verify=no` / `-tls_verify 0`). Never use it over the internet: it allows anyone on the path to read the token and the stream.
If the server answers `http://` with a redirect to `https://`, the CLI stops with a message telling you to use the https URL (a redirect would otherwise drop the login request).
A server URL without a scheme is treated as `https://`.
