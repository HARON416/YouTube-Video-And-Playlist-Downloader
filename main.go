// ytgo downloads YouTube videos and playlists using a bounded yt-dlp worker pool.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type config struct {
	mode, outDir, archive, url, jsRuntimes string
	networkRetries, fragmentRetries        string
	socketTimeout                          int
	workers, height, attempts              int
	single, verbose                        bool
	retryDelay                             time.Duration
	updates, updateChannel, updateCache    string
	updateOnly                             bool
	updateSudo                             bool
	updateTimeout                          time.Duration
}

func parseConfig(args []string, output io.Writer) (config, error) {
	c := config{}
	var quality string
	f := flag.NewFlagSet("ytgo", flag.ContinueOnError)
	f.SetOutput(output)
	f.StringVar(&c.mode, "mode", "video", "video (MP4) or audio (MP3); aliases: mp4, mp3")
	f.StringVar(&c.outDir, "o", "~/Videos", "base output directory")
	f.StringVar(&c.archive, "archive", "", "archive path (default: <destination>/.ytgo-archive.txt)")
	f.StringVar(&c.jsRuntimes, "js-runtimes", "", "yt-dlp JavaScript runtimes, e.g. node or deno:/path/to/deno")
	f.IntVar(&c.workers, "j", 3, "maximum concurrent video downloads")
	f.StringVar(&quality, "q", "1080", "maximum video height, or best for no resolution limit")
	f.IntVar(&c.socketTimeout, "socket-timeout", 60, "network socket timeout in seconds")
	f.StringVar(&c.networkRetries, "network-retries", "3", "yt-dlp network retries per attempt: nonnegative integer or infinite")
	f.StringVar(&c.fragmentRetries, "fragment-retries", "3", "yt-dlp fragment retries per attempt: nonnegative integer or infinite")
	f.IntVar(&c.attempts, "retries", 5, "total attempts per video, including the first")
	f.DurationVar(&c.retryDelay, "retry-delay", 5*time.Second, "base delay between attempts (increases per attempt)")
	f.BoolVar(&c.single, "single", false, "download only the video when a URL also contains a playlist")
	f.BoolVar(&c.verbose, "v", false, "stream yt-dlp output (workers may interleave)")
	f.StringVar(&c.updates, "updates", "daily", "dependency updates: daily, always, or off")
	f.StringVar(&c.updateChannel, "update-channel", "nightly", "yt-dlp update channel: nightly or stable")
	f.StringVar(&c.updateCache, "update-cache", "", "update state file (default: user cache directory/ytgo/updates.json)")
	f.DurationVar(&c.updateTimeout, "update-timeout", 2*time.Minute, "maximum time per dependency update")
	f.BoolVar(&c.updateOnly, "update-only", false, "force dependency updates and exit; omit URL")
	f.BoolVar(&c.updateSudo, "update-sudo", false, "allow sudo to prompt and retry updates that fail with permission errors")
	f.Usage = func() {
		fmt.Fprintln(output, "Usage: ytgo [options] URL\n       ytgo -update-only [options]\nOptions must come before URL.")
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		return c, err
	}
	switch c.mode {
	case "mp3":
		c.mode = "audio"
	case "mp4":
		c.mode = "video"
	}
	if quality != "best" {
		height, err := strconv.Atoi(quality)
		if err != nil || height < 1 {
			return c, errors.New("-q must be a positive height or best")
		}
		c.height = height
	}
	wantArgs := 1
	if c.updateOnly {
		wantArgs = 0
	}
	if (c.updates != "daily" && c.updates != "always" && c.updates != "off") || (c.updateChannel != "stable" && c.updateChannel != "nightly") || c.updateTimeout <= 0 {
		return c, errors.New("use -updates daily|always|off, -update-channel nightly|stable, and positive -update-timeout")
	}
	if f.NArg() != wantArgs || (c.mode != "video" && c.mode != "audio") || c.workers < 1 || c.attempts < 1 || c.retryDelay < 0 || c.socketTimeout < 1 || !validRetries(c.networkRetries) || !validRetries(c.fragmentRetries) {
		return c, errors.New("provide one URL, valid mode, positive -j/-retries/-socket-timeout, nonnegative -retry-delay, and retry counts as nonnegative integers or infinite; see -help")
	}
	c.url = f.Arg(0)
	c.outDir = expandHome(c.outDir)
	c.archive = expandHome(c.archive)
	return c, nil
}

func validRetries(value string) bool {
	if value == "infinite" {
		return true
	}
	n, err := strconv.Atoi(value)
	return err == nil && n >= 0
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

var badChars = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)

func sanitize(s string) string {
	s = strings.Trim(badChars.ReplaceAllString(s, "_"), " .")
	if s == "" {
		return "playlist"
	}
	return s
}

type entry struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}
type metadata struct {
	entry
	Type    string   `json:"_type"`
	Entries []*entry `json:"entries"`
}

func commonArgs(c config) []string {
	a := []string{"--ignore-config", "--socket-timeout", strconv.Itoa(c.socketTimeout), "--retries", c.networkRetries, "--fragment-retries", c.fragmentRetries}
	if c.jsRuntimes != "" {
		a = append(a, "--js-runtimes", c.jsRuntimes)
	}
	return a
}

func resolve(ctx context.Context, c config) (string, []entry, error) {
	a := append(commonArgs(c), "--dump-single-json", "--flat-playlist")
	if c.single {
		a = append(a, "--no-playlist")
	} else {
		a = append(a, "--yes-playlist")
	}
	a = append(a, "--", c.url)
	cmd := exec.CommandContext(ctx, "yt-dlp", a...)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("resolve URL: %w: %s", err, stderr.String())
	}
	var m metadata
	if err := json.Unmarshal(out, &m); err != nil {
		return "", nil, fmt.Errorf("parse metadata: %w", err)
	}
	if m.Entries == nil && m.Type != "playlist" && m.Type != "multi_video" {
		if m.ID == "" {
			return "", nil, errors.New("URL returned no video ID")
		}
		return "", []entry{m.entry}, nil
	}
	entries := make([]entry, 0, len(m.Entries))
	seen := map[string]bool{}
	for _, e := range m.Entries {
		if e != nil && e.ID != "" && !seen[e.ID] {
			entries = append(entries, *e)
			seen[e.ID] = true
		}
	}
	return sanitize(m.Title), entries, nil
}

// Keep diagnostics bounded even for long downloads. exec may write concurrently.
type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	const limit = 16 * 1024
	n := len(p)
	if len(p) >= limit {
		b.data = append(b.data[:0], p[len(p)-limit:]...)
	} else {
		b.data = append(b.data, p...)
		if len(b.data) > limit {
			b.data = b.data[len(b.data)-limit:]
		}
	}
	return n, nil
}
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.data))
}

func downloadArgs(c config, dir, id string) []string {
	template := "%(title)s [%(id)s].%(ext)s"
	if c.mode == "video" {
		template = fmt.Sprintf("%%(title)s [%%(id)s] [max-%dp].%%(ext)s", c.height)
		if c.height == 0 {
			template = "%(title)s [%(id)s] [best].%(ext)s"
		}
	}
	a := append(commonArgs(c), "--no-playlist", "--newline", "--continue", "--abort-on-error",
		"--abort-on-unavailable-fragments",
		"--paths", dir, "-o", template)
	if c.mode == "audio" {
		a = append(a, "-x", "--audio-format", "mp3", "--audio-quality", "0", "--embed-thumbnail", "--embed-metadata")
	} else {
		format := "bv*+ba/b"
		if c.height > 0 {
			format = fmt.Sprintf("bv*[height<=%d]+ba/b[height<=%d]", c.height, c.height)
		}
		a = append(a, "-f", format, "--merge-output-format", "mp4", "--remux-video", "mp4")
	}
	return append(a, "--", "https://www.youtube.com/watch?v="+id)
}

func download(ctx context.Context, c config, dir, id string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, "yt-dlp", downloadArgs(c, dir, id)...)
	var tail tailBuffer
	var writer io.Writer = &tail
	if c.verbose {
		writer = io.MultiWriter(&tail, output)
	}
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, tail.String())
	}
	return nil
}

type archive struct {
	mu   sync.Mutex
	path string
	ids  map[string]bool
}

func loadArchive(path string) (*archive, error) {
	a := &archive{path: path, ids: map[string]bool{}}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		a.ids[strings.TrimSpace(line)] = true
	}
	return a, nil
}
func archiveKey(c config, id string) string {
	// A higher resolution request must not be skipped by an earlier lower one.
	if c.mode == "video" {
		if c.height == 0 {
			return "ytgo-video-best " + id
		}
		return "ytgo-video-" + strconv.Itoa(c.height) + " " + id
	}
	return "ytgo-audio " + id
}
func (a *archive) has(key string) bool { a.mu.Lock(); defer a.mu.Unlock(); return a.ids[key] }
func (a *archive) add(key string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ids[key] {
		return nil
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintln(f, key)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	a.ids[key] = true
	return nil
}

// synchronizedWriter also protects custom writers supplied by callers/tests.
type synchronizedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *synchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

func run(ctx context.Context, c config, output io.Writer) error {
	output = &synchronizedWriter{writer: output}
	l := log.New(output, "", log.Ltime)
	if err := prepareDependencies(ctx, c, l); err != nil {
		return err
	}
	if c.updateOnly {
		return nil
	}
	folder, entries, err := resolve(ctx, c)
	if err != nil {
		return err
	}
	dir := filepath.Join(c.outDir, folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if c.archive == "" {
		c.archive = filepath.Join(dir, ".ytgo-archive.txt")
	}
	if err := os.MkdirAll(filepath.Dir(c.archive), 0o755); err != nil {
		return err
	}
	a, err := loadArchive(c.archive)
	if err != nil {
		return fmt.Errorf("read archive: %w", err)
	}
	var todo []entry
	for _, e := range entries {
		if !a.has(archiveKey(c, e.ID)) {
			todo = append(todo, e)
		}
	}
	l.Printf("%d to download, %d archived; up to %d workers -> %s", len(todo), len(entries)-len(todo), c.workers, dir)
	jobs := make(chan entry)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, failed := 0, 0
	for w := 0; w < c.workers && w < len(todo); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range jobs {
				var lastErr error
				for attempt := 1; attempt <= c.attempts; attempt++ {
					if ctx.Err() != nil {
						return
					}
					lastErr = download(ctx, c, dir, e.ID, output)
					if lastErr == nil || ctx.Err() != nil {
						break
					}
					if attempt < c.attempts {
						l.Printf("retry %d/%d for %s: %v", attempt, c.attempts, e.ID, lastErr)
						timer := time.NewTimer(c.retryDelay * time.Duration(attempt))
						select {
						case <-ctx.Done():
							timer.Stop()
							return
						case <-timer.C:
						}
					}
				}
				if ctx.Err() != nil {
					return
				}
				if lastErr == nil {
					lastErr = a.add(archiveKey(c, e.ID))
				}
				mu.Lock()
				if lastErr != nil {
					failed++
					l.Printf("FAILED %s (%s): %v", e.Title, e.ID, lastErr)
				} else {
					ok++
					l.Printf("done: %s (%s)", e.Title, e.ID)
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, e := range todo {
		select {
		case <-ctx.Done():
			break feed
		case jobs <- e:
		}
	}
	close(jobs)
	wg.Wait()
	l.Printf("finished: %d ok, %d failed, %d skipped", ok, failed, len(entries)-len(todo))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failed > 0 {
		return fmt.Errorf("%d downloads or archive writes failed; re-run to retry", failed)
	}
	return nil
}

func main() {
	c, err := parseConfig(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, c, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "interrupted; re-run the same command to resume")
			os.Exit(130)
		}
		os.Exit(1)
	}
}
