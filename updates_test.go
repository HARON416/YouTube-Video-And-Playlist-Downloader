package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func updateTools(t *testing.T) (config, *bytes.Buffer, *log.Logger) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("YTGO_UPDATE_DIR", dir)
	script := `#!/bin/sh
name=${0##*/}
case "$*" in
  *--version*|*-version*)
    if [ -f "$YTGO_UPDATE_DIR/$name.version" ]; then /bin/cat "$YTGO_UPDATE_DIR/$name.version"; else echo "$name old"; fi
    exit 0;;
esac
printf '%s %s\n' "$name" "$*" >> "$YTGO_UPDATE_DIR/calls"
if [ "$YTGO_UPDATE_SLOW" = "$name" ]; then /bin/sleep 10; fi
if [ "$YTGO_UPDATE_FAIL" = "$name" ]; then echo "${YTGO_UPDATE_ERROR:-update denied}" >&2; exit 1; fi
echo "$name new" > "$YTGO_UPDATE_DIR/$name.version"
`
	for _, name := range []string{"yt-dlp", "ffmpeg", "deno", "node"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := testConfig(t)
	c.updates = "daily"
	c.updateCache = filepath.Join(dir, "cache", "updates.json")
	output := &bytes.Buffer{}
	return c, output, log.New(output, "", 0)
}

func updateCalls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("YTGO_UPDATE_DIR"), "calls"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(data)
}

func TestDailyUpdatesCacheAndForce(t *testing.T) {
	c, _, l := updateTools(t)
	if err := prepareDependencies(context.Background(), c, l); err != nil {
		t.Fatal(err)
	}
	calls := updateCalls(t)
	if strings.Count(calls, "yt-dlp --ignore-config --update-to nightly") != 1 || strings.Count(calls, "deno upgrade") != 1 || strings.Contains(calls, "ffmpeg") {
		t.Fatal(calls)
	}
	if err := prepareDependencies(context.Background(), c, l); err != nil {
		t.Fatal(err)
	}
	if updateCalls(t) != calls {
		t.Fatal("repeated successful daily updates")
	}
	c.updateOnly = true
	if err := prepareDependencies(context.Background(), c, l); err != nil {
		t.Fatal(err)
	}
	if strings.Count(updateCalls(t), "deno upgrade") != 2 {
		t.Fatal("force used cache")
	}
	c.updateOnly = false
	c.updates = "always"
	if err := prepareDependencies(context.Background(), c, l); err != nil {
		t.Fatal(err)
	}
	if strings.Count(updateCalls(t), "deno upgrade") != 3 {
		t.Fatal("always used cache")
	}
}

func TestUpdatesOffAndMissingTools(t *testing.T) {
	c, _, l := updateTools(t)
	c.updates = "off"
	if err := prepareDependencies(context.Background(), c, l); err != nil {
		t.Fatal(err)
	}
	if updateCalls(t) != "" {
		t.Fatal("off updated dependencies")
	}
	if err := os.Rename(filepath.Join(os.Getenv("YTGO_UPDATE_DIR"), "deno"), filepath.Join(os.Getenv("YTGO_UPDATE_DIR"), "deno.disabled")); err != nil {
		t.Fatal(err)
	}
	if err := prepareDependencies(context.Background(), c, l); err == nil || !strings.Contains(err.Error(), "deno not found") {
		t.Fatalf("missing dependency: %v", err)
	}
}

func TestUpdateFailureRetriesOnlyFailedTool(t *testing.T) {
	c, output, l := updateTools(t)
	t.Setenv("YTGO_UPDATE_FAIL", "yt-dlp")
	for i := 0; i < 2; i++ {
		if err := prepareDependencies(context.Background(), c, l); err != nil {
			t.Fatal(err)
		}
	}
	calls := updateCalls(t)
	if strings.Count(calls, "yt-dlp --ignore-config") != 2 || strings.Count(calls, "deno upgrade") != 1 {
		t.Fatal(calls)
	}
	if !strings.Contains(output.String(), "continuing with installed tools") {
		t.Fatal(output.String())
	}
	c.updateOnly = true
	if err := prepareDependencies(context.Background(), c, l); err == nil {
		t.Fatal("explicit update hid failure")
	}
}

func TestExpiredCacheAndChangedChannel(t *testing.T) {
	c, _, l := updateTools(t)
	if err := prepareDependencies(context.Background(), c, l); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.updateCache)
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]updateRecord{}
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatal(err)
	}
	for key, record := range records {
		record.Checked = time.Now().Add(-25 * time.Hour)
		records[key] = record
	}
	if err := saveUpdateRecords(c.updateCache, records); err != nil {
		t.Fatal(err)
	}
	if err := prepareDependencies(context.Background(), c, l); err != nil {
		t.Fatal(err)
	}
	if strings.Count(updateCalls(t), "deno upgrade") != 2 {
		t.Fatal("expired cache used")
	}
	c.updateChannel = "stable"
	if err := prepareDependencies(context.Background(), c, l); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updateCalls(t), "--update-to stable") || strings.Count(updateCalls(t), "deno upgrade") != 2 {
		t.Fatal(updateCalls(t))
	}
}

func TestChangedVersionInvalidatesCache(t *testing.T) {
	c, _, l := updateTools(t)
	if err := prepareDependencies(context.Background(), c, l); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("YTGO_UPDATE_DIR"), "deno.version"), []byte("deno replaced"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareDependencies(context.Background(), c, l); err != nil {
		t.Fatal(err)
	}
	if strings.Count(updateCalls(t), "deno upgrade") != 2 {
		t.Fatal("replaced version kept cached freshness")
	}
}

func TestUpdateCancellation(t *testing.T) {
	c, _, l := updateTools(t)
	t.Setenv("YTGO_UPDATE_SLOW", "yt-dlp")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := prepareDependencies(ctx, c, l); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancellation did not stop updater")
	}
}

func TestUpdateCommandTimeout(t *testing.T) {
	c, output, l := updateTools(t)
	t.Setenv("YTGO_UPDATE_SLOW", "yt-dlp")
	c.updateTimeout = 100 * time.Millisecond
	if err := prepareDependencies(context.Background(), c, l); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "deadline exceeded") || !strings.Contains(updateCalls(t), "deno upgrade") {
		t.Fatal(output.String())
	}
}

func TestCustomNodeRuntime(t *testing.T) {
	c, output, l := updateTools(t)
	c.jsRuntimes = "node:" + filepath.Join(os.Getenv("YTGO_UPDATE_DIR"), "node")
	if err := prepareDependencies(context.Background(), c, l); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(updateCalls(t), "deno") || strings.Contains(updateCalls(t), "node") || !strings.Contains(output.String(), "node updates are managed externally") {
		t.Fatal(updateCalls(t), output.String())
	}
}

func TestLockCancellation(t *testing.T) {
	c, _, _ := updateTools(t)
	lock, err := lockUpdateCache(context.Background(), c.updateCache)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := lockUpdateCache(ctx, c.updateCache); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock: %v", err)
	}
}

func TestUpdateFlags(t *testing.T) {
	c, err := parseConfig([]string{"-update-only"}, &bytes.Buffer{})
	if err != nil || !c.updateOnly || c.updates != "daily" || c.updateChannel != "nightly" {
		t.Fatalf("%+v %v", c, err)
	}
	for _, args := range [][]string{{"-updates", "invalid", "url"}, {"-update-channel", "master", "url"}, {"-update-timeout", "0s", "url"}, {"-update-only", "url"}} {
		if _, err := parseConfig(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestDownloadContinuesAfterUpdateFailure(t *testing.T) {
	dir := fakeTools(t, `{"id":"one","title":"One"}`)
	path := filepath.Join(dir, "yt-dlp")
	script, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script = bytes.Replace(script, []byte("#!/bin/sh\n"), []byte("#!/bin/sh\ncase \"$*\" in *--update-to*) echo 'network unavailable' >&2; exit 1;; esac\n"), 1)
	if err := os.WriteFile(path, script, 0o755); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t)
	c.updates = "daily"
	c.updateCache = filepath.Join(dir, "cache", "updates.json")
	var output bytes.Buffer
	if err := run(context.Background(), c, &output); err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile(filepath.Join(dir, "events"))
	if err != nil || !strings.Contains(string(events), "start one") || !strings.Contains(output.String(), "continuing with installed tools") {
		t.Fatalf("download fallback: %s %v\n%s", events, err, output.String())
	}
}

func TestUpdateOnlyDoesNotDownload(t *testing.T) {
	c, _, _ := updateTools(t)
	c.updateOnly = true
	if err := run(context.Background(), c, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(updateCalls(t), "watch?v=") || strings.Contains(updateCalls(t), "--dump-single-json") {
		t.Fatal(updateCalls(t))
	}
}

func fakeSudo(t *testing.T) {
	t.Helper()
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$YTGO_UPDATE_DIR/sudo-calls"
if [ "$YTGO_SUDO_FAIL" = yes ]; then exit 1; fi
[ "$1" = -- ] || exit 2
shift
unset YTGO_UPDATE_FAIL
exec "$@"
`
	if err := os.WriteFile(filepath.Join(os.Getenv("YTGO_UPDATE_DIR"), "sudo"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestPermissionFailureUsesSudoWhenEnabled(t *testing.T) {
	c, output, l := updateTools(t)
	fakeSudo(t)
	c.updateSudo = true
	c.updateOnly = true
	t.Setenv("YTGO_UPDATE_FAIL", "yt-dlp")
	t.Setenv("YTGO_UPDATE_ERROR", "ERROR: Insufficient permissions to write to /usr/local/bin/yt-dlp")
	if err := prepareDependencies(context.Background(), c, l); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(filepath.Join(os.Getenv("YTGO_UPDATE_DIR"), "sudo-calls"))
	want := "-- " + filepath.Join(os.Getenv("YTGO_UPDATE_DIR"), "yt-dlp") + " --ignore-config --update-to nightly\n"
	if err != nil || string(calls) != want {
		t.Fatalf("sudo target/arguments: %q, %v", calls, err)
	}
	if !strings.Contains(output.String(), "retrying with sudo") {
		t.Fatal(output.String())
	}
	// The elevated update's success must populate the normal user's cache.
	c.updateOnly = false
	if err := prepareDependencies(context.Background(), c, l); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(os.Getenv("YTGO_UPDATE_DIR"), "sudo-calls"))
	if string(again) != string(calls) {
		t.Fatal("elevated success was not cached")
	}
}

func TestNoSudoForDisabledOrUnrelatedFailure(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			c, _, l := updateTools(t)
			fakeSudo(t)
			c.updateOnly = true
			c.updateSudo = enabled
			t.Setenv("YTGO_UPDATE_FAIL", "yt-dlp")
			message := "Permission denied"
			if enabled {
				message = "network unavailable"
			}
			t.Setenv("YTGO_UPDATE_ERROR", message)
			if err := prepareDependencies(context.Background(), c, l); err == nil {
				t.Fatal("failure hidden")
			}
			if _, err := os.Stat(filepath.Join(os.Getenv("YTGO_UPDATE_DIR"), "sudo-calls")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unexpected sudo invocation")
			}
		})
	}
}

func TestSudoFailureIsReported(t *testing.T) {
	c, _, l := updateTools(t)
	fakeSudo(t)
	c.updateOnly, c.updateSudo = true, true
	t.Setenv("YTGO_UPDATE_FAIL", "yt-dlp")
	t.Setenv("YTGO_UPDATE_ERROR", "Permission denied")
	t.Setenv("YTGO_SUDO_FAIL", "yes")
	if err := prepareDependencies(context.Background(), c, l); err == nil {
		t.Fatal("sudo denial returned success")
	}
}
