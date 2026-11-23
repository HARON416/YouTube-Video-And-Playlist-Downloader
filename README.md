# ytgo

ytgo is a Go wrapper around yt-dlp for downloading YouTube videos and playlists as MP4 video or MP3 audio. Its main benefits are a simpler command interface and concurrent playlist downloads: one command selects the format and quality, while a worker pool downloads several videos at once.

The wrapper resolves playlist entries, creates a folder from the playlist title, and distributes videos across configurable workers. It retries failed videos, records successful downloads, and reports completed, failed, and skipped items. Single-video URLs work through the same command.

## Benefits

- **Ease of use:** choose format, quality, concurrency, and destination through readable Make parameters instead of assembling long yt-dlp commands.
- **Concurrent downloads:** process several playlist videos at once to improve throughput when the connection and YouTube allow it.
- **Retries and resuming:** restart failed yt-dlp attempts with fresh media URLs, skip archived successes, and resume available partial files when rerunning a command.
- **Separate download tracking:** distinguish MP3, MP4, and requested video quality settings, so an earlier audio or lower-quality download does not block a different request.
- **Dependency management:** validate installed tools, check for yt-dlp and Deno updates daily, and support sudo prompts when enabled. FFmpeg upgrades use a separate Ubuntu command.

## Compared with yt-dlp directly

| Task | Raw yt-dlp in the terminal | ytgo |
| --- | --- | --- |
| Configure downloads | Supply flags or maintain a yt-dlp configuration file. | Use one `make download` command with format and quality parameters. |
| Download a playlist | A normal playlist command processes entries sequentially. | Run multiple video downloads concurrently through a worker pool. |
| Retry failures | Use yt-dlp's internal retries; add a shell loop for repeated process attempts. | Combine internal retries with configurable per-video process attempts. |
| Skip completed downloads | Configure a download archive. | Automatically track successes by format and quality in the destination archive. |
| Maintain tools | Run update commands separately. | Check/update yt-dlp and Deno before downloads, with successful checks cached for 24 hours. |

yt-dlp remains the download engine. Extraction, quality selection, MP3 conversion, merging, and partial-file resuming come from yt-dlp and FFmpeg; ytgo coordinates them. It ignores personal yt-dlp configuration and exposes a focused set of options; use yt-dlp directly when you need its broader controls.

Concurrency shares available bandwidth, so more workers do not guarantee more speed. This wrapper parallelizes separate videos rather than accelerating a single video. Failed items stop after the configured attempts; the application does not retry everything forever.

## Commands

| Command | What it does |
| --- | --- |
| `make download URL='URL'` | Download a single video or playlist as MP4; build automatically. Use `FORMAT=mp3` for audio or `QUALITY=best` for uncapped video quality. |
| `make update` | Update yt-dlp and Deno; prompt through sudo on permission failures. |
| `make update-system` | Upgrade Ubuntu FFmpeg packages through sudo. |
| `make check` | Run race tests, vet, and formatting checks. |

```bash
# Download with every optional parameter (shown with defaults).
# FORMAT: mp4 or mp3; SINGLE=true ignores playlist parameters.
# JOBS: concurrent videos; QUALITY: maximum height or best.
# ARCHIVE/UPDATE_CACHE: empty selects the default destination archive/user cache.
# RETRIES: total attempts; RETRY_DELAY: base delay between attempts.
# SOCKET_TIMEOUT: seconds per network operation.
# NETWORK_RETRIES/FRAGMENT_RETRIES: nonnegative integer or infinite.
# JS_RUNTIMES: empty uses Deno; node selects Node.
# VERBOSE=true streams downloader output.
# UPDATES: daily, always, off; UPDATE_CHANNEL: nightly or stable.
# UPDATE_TIMEOUT: time limit per update; UPDATE_SUDO=true permits sudo prompts.
make download URL='URL' \
  FORMAT=mp4 \
  SINGLE=false \
  JOBS=3 \
  QUALITY=1080 \
  OUT='./downloads' \
  ARCHIVE='' \
  RETRIES=5 \
  RETRY_DELAY=5s \
  SOCKET_TIMEOUT=60 \
  NETWORK_RETRIES=3 \
  FRAGMENT_RETRIES=3 \
  JS_RUNTIMES='' \
  VERBOSE=true \
  UPDATES=daily \
  UPDATE_CHANNEL=nightly \
  UPDATE_TIMEOUT=5m \
  UPDATE_SUDO=true \
  UPDATE_CACHE=''
```

## Playlist download example

```bash
make download URL='https://youtube.com/playlist?list=RDNFjRwHo19zY&playnext=1&si=YJVjeLKebg8ZzkXO' \
  FORMAT=mp4 \
  SINGLE=false \
  JOBS=10 \
  QUALITY=1080 \
  OUT='./downloads' \
  ARCHIVE='' \
  RETRIES=5 \
  RETRY_DELAY=5s \
  SOCKET_TIMEOUT=60 \
  NETWORK_RETRIES=3 \
  FRAGMENT_RETRIES=3 \
  JS_RUNTIMES='' \
  VERBOSE=true \
  UPDATES=daily \
  UPDATE_CHANNEL=nightly \
  UPDATE_TIMEOUT=5m \
  UPDATE_SUDO=true \
  UPDATE_CACHE=''
```

| Setting | What it does in this example |
| --- | --- |
| `URL` | Resolve the videos returned by this YouTube Mix; its contents and title can change between runs. |
| `FORMAT=mp4` | Download MP4 video; use `mp3` for audio. |
| `SINGLE=false` | Process the playlist. A URL with no playlist still downloads just one video. |
| `JOBS=10` | Run up to 10 video downloads concurrently, sharing available bandwidth. |
| `QUALITY=1080` | Limit video height to 1080p; use `best` to remove the cap. |
| `OUT='./downloads'` | Save files under `./downloads/<detected playlist title>/`. |
| `ARCHIVE=''` | Use the destination's `.ytgo-archive.txt`; skip archived successes for the same format and quality setting. |
| `RETRIES=5` | Allow five total ytgo attempts per video, including the first. |
| `RETRY_DELAY=5s` | Wait 5, 10, 15, then 20 seconds between failed attempts. |
| `SOCKET_TIMEOUT=60` | Wait up to 60 seconds for stalled network operations; this is not a total download deadline. |
| `NETWORK_RETRIES=3` | Allow three internal yt-dlp retries per applicable network operation in each attempt; also accepts `infinite`. |
| `FRAGMENT_RETRIES=3` | Allow three retries per failed media fragment in each attempt; also accepts `infinite`. |
| `JS_RUNTIMES=''` | Use Deno; set `node` to use Node. |
| `VERBOSE=true` | Stream downloader output; concurrent workers' messages may interleave. |
| `UPDATES=daily` | Validate installed tools every run and cache successful yt-dlp/Deno update checks for 24 hours. Failed updates warn and continue with installed tools. Also accepts `always` or `off`. |
| `UPDATE_CHANNEL=nightly` | Update yt-dlp from its nightly channel; also accepts `stable`. |
| `UPDATE_TIMEOUT=5m` | Allow up to five minutes per dependency update command and update-cache lock wait. |
| `UPDATE_SUDO=true` | Retry update permission failures through sudo's terminal password prompt. |
| `UPDATE_CACHE=''` | Use `ytgo/updates.json` under the default user cache directory. |
| Completion and resuming | Finish after processing the queue; exhausted failures return an error. Re-run the same command to skip archived successes and resume available partial files. |
| FFmpeg updates | Validate FFmpeg before downloading; upgrade it separately with `make update-system`. |
# youtube-playlist-downloader-go
