package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfig(t *testing.T) {
	for _, args := range [][]string{{"-j", "0", "url"}, {"-q", "0", "url"}, {"-retries", "0", "url"}, {"-mode", "wav", "url"}, {"-retry-delay", "-1s", "url"}, {"url", "extra"}} {
		if _, err := parseConfig(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	c, err := parseConfig([]string{"-mode", "mp3", "url"}, &bytes.Buffer{})
	if err != nil || c.mode != "audio" {
		t.Fatalf("alias: %+v %v", c, err)
	}
}

func TestSafeFolder(t *testing.T) {
	for _, title := range []string{"..", ".", " / ", "../../music", "a\x00b"} {
		folder := sanitize(title)
		if folder == "." || folder == ".." || strings.ContainsAny(folder, "/\\\x00") {
			t.Fatalf("unsafe: %q", folder)
		}
	}
}

// This fake external process exercises actual exec calls, without network downloads.
func fakeTools(t *testing.T, metadata string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("YTGO_TEST_DIR", dir)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("metadata.json", metadata)
	write("ffmpeg", "#!/bin/sh\necho 'ffmpeg test-version'\n")
	write("deno", "#!/bin/sh\necho 'deno test-version'\n")
	write("yt-dlp", `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "--version" ]; then
    echo test-version
    exit 0
  fi
done
for arg in "$@"; do
  if [ "$arg" = "--dump-single-json" ]; then
    printf '%s\n' "$@" > "$YTGO_TEST_DIR/resolve-args"
    cat "$YTGO_TEST_DIR/metadata.json"
    exit 0
  fi
done
for last in "$@"; do :; done
id=${last##*=}
printf 'start %s\n' "$id" >> "$YTGO_TEST_DIR/events"
sleep 0.1
if [ "$id" = "fail" ]; then
  echo 'simulated network failure' >&2
  printf 'end %s\n' "$id" >> "$YTGO_TEST_DIR/events"
  exit 1
fi
printf 'end %s\n' "$id" >> "$YTGO_TEST_DIR/events"
`)
	return dir
}

func TestSingleVideoResolution(t *testing.T) {
	dir := fakeTools(t, `{"id":"one","title":"One"}`)
	c := testConfig(t)
	c.single = true
	folder, entries, err := resolve(context.Background(), c)
	if err != nil || folder != "" || len(entries) != 1 || entries[0].ID != "one" {
		t.Fatalf("%q %v %v", folder, entries, err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "resolve-args"))
	if err != nil || !strings.Contains(string(args), "--no-playlist") || strings.Contains(string(args), "--yes-playlist") {
		t.Fatalf("single resolution args: %s, %v", args, err)
	}
}

func testConfig(t *testing.T) config {
	return config{mode: "video", outDir: t.TempDir(), workers: 2, height: 1080, attempts: 2, socketTimeout: 60, networkRetries: "3", fragmentRetries: "3", updates: "off", updateChannel: "nightly", updateTimeout: time.Second, url: "https://youtube.com/playlist?list=test"}
}

func TestNetworkAndBestQualityOptions(t *testing.T) {
	c, err := parseConfig([]string{"-q", "best", "-socket-timeout", "90", "-network-retries", "infinite", "-fragment-retries", "infinite", "url"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if c.height != 0 {
		t.Fatalf("best quality capped at %d", c.height)
	}
	args := downloadArgs(c, "/tmp/output", "one")
	want := map[string]string{"-f": "bv*+ba/b", "--socket-timeout": "90", "--retries": "infinite", "--fragment-retries": "infinite"}
	for option, value := range want {
		found := 0
		for i, arg := range args {
			if arg == option {
				found++
				if i+1 >= len(args) || args[i+1] != value {
					t.Fatalf("%s: %v", option, args)
				}
			}
		}
		if found != 1 {
			t.Fatalf("%s count=%d: %v", option, found, args)
		}
	}
	if !strings.Contains(strings.Join(args, " "), "[best]") {
		t.Fatal("missing best filename marker")
	}
	capped := c
	capped.height = 1080
	if archiveKey(c, "one") == archiveKey(capped, "one") {
		t.Fatal("best shares capped archive key")
	}
	dir := fakeTools(t, `{"id":"one"}`)
	if _, _, err := resolve(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	resolveArgs, err := os.ReadFile(filepath.Join(dir, "resolve-args"))
	if err != nil || !strings.Contains(string(resolveArgs), "--socket-timeout\n90\n") {
		t.Fatalf("resolve timeout: %s %v", resolveArgs, err)
	}
}

func TestInvalidNetworkAndQualityOptions(t *testing.T) {
	for _, args := range [][]string{
		{"-q", "-1", "url"}, {"-q", "auto", "url"},
		{"-socket-timeout", "0", "url"}, {"-socket-timeout", "-10", "url"},
		{"-network-retries", "-1", "url"}, {"-fragment-retries", "forever", "url"},
	} {
		if _, err := parseConfig(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if _, err := parseConfig([]string{"-network-retries", "0", "-fragment-retries", "0", "url"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("zero retries rejected: %v", err)
	}
}

func TestPlaylistConcurrencyAndArchive(t *testing.T) {
	dir := fakeTools(t, `{"_type":"playlist","title":"My / Mix","entries":[{"id":"one"},{"id":"two"},{"id":"three"},{"id":"one"},null]}`)
	c := testConfig(t)
	var output bytes.Buffer
	if err := run(context.Background(), c, &output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "events"))
	if err != nil {
		t.Fatal(err)
	}
	active, peak, started := 0, 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "start") {
			active++
			started++
		} else {
			active--
		}
		if active > peak {
			peak = active
		}
	}
	if active != 0 || peak != 2 || started != 3 {
		t.Fatalf("active=%d peak=%d started=%d\n%s", active, peak, started, data)
	}
	if err := run(context.Background(), c, &output); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(dir, "events"))
	if string(again) != string(data) {
		t.Fatal("archived downloads ran again")
	}
	c.mode = "audio"
	if err := run(context.Background(), c, &output); err != nil {
		t.Fatal(err)
	}
	again, _ = os.ReadFile(filepath.Join(dir, "events"))
	if strings.Count(string(again), "start") != 6 {
		t.Fatal("video archive blocked audio")
	}
}

func TestFailuresRetryWithoutArchiving(t *testing.T) {
	dir := fakeTools(t, `{"id":"fail","title":"Unavailable"}`)
	c := testConfig(t)
	var output bytes.Buffer
	if err := run(context.Background(), c, &output); err == nil {
		t.Fatal("failure returned success")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "events"))
	if strings.Count(string(data), "start") != c.attempts {
		t.Fatalf("attempts: %s", data)
	}
	a, err := loadArchive(filepath.Join(c.outDir, ".ytgo-archive.txt"))
	if err != nil || a.has(archiveKey(c, "fail")) {
		t.Fatal("failure archived")
	}
	if !strings.Contains(output.String(), "simulated network failure") {
		t.Fatal("missing diagnostic")
	}
}

func TestCancellationDuringBackoff(t *testing.T) {
	fakeTools(t, `{"id":"fail"}`)
	c := testConfig(t)
	c.retryDelay = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := run(ctx, c, &bytes.Buffer{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancellation did not interrupt backoff")
	}
}

func TestEmptyPlaylist(t *testing.T) {
	fakeTools(t, `{"_type":"playlist","id":"playlist-id","title":"Empty","entries":[]}`)
	c := testConfig(t)
	folder, entries, err := resolve(context.Background(), c)
	if err != nil || folder != "Empty" || len(entries) != 0 {
		t.Fatalf("%q %v %v", folder, entries, err)
	}
}

func TestDownloadModes(t *testing.T) {
	c := testConfig(t)
	args := strings.Join(downloadArgs(c, "/tmp/output%dir", "abc"), " ")
	if !strings.Contains(args, "height<=1080") || !strings.Contains(args, "--remux-video mp4") || strings.Contains(args, "--audio-format") {
		t.Fatal(args)
	}
	c.mode = "audio"
	args = strings.Join(downloadArgs(c, "/tmp/output", "abc"), " ")
	if !strings.Contains(args, "--audio-format mp3") || strings.Contains(args, "--remux-video") {
		t.Fatal(args)
	}
	video := c
	video.mode = "video"
	if archiveKey(c, "abc") == archiveKey(video, "abc") {
		t.Fatal("formats share archive key")
	}
}

func TestConcurrentArchiveWrites(t *testing.T) {
	a, err := loadArchive(filepath.Join(t.TempDir(), "archive"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := a.add("ytgo-audio " + strconv.Itoa(i%10)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	data, err := os.ReadFile(a.path)
	if err != nil || strings.Count(string(data), "\n") != 10 {
		t.Fatalf("archive: %s, %v", data, err)
	}
}

func TestArchiveReadError(t *testing.T) {
	if _, err := loadArchive(t.TempDir()); err == nil {
		t.Fatal("silenced archive read failure")
	}
}
