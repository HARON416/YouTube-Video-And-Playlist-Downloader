package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type dependency struct {
	name, path, version     string
	versionArgs, updateArgs []string
}

type updateRecord struct {
	Version string    `json:"version"`
	Checked time.Time `json:"checked"`
}

func dependencyCommand(ctx context.Context, timeout time.Duration, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	// Kill descendants too, so cancellation cannot leave an updater running.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	var output tailBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return output.String(), err
}

func permissionFailure(output string) bool {
	output = strings.ToLower(output)
	for _, message := range []string{"insufficient permissions", "permission denied", "not writable", "not writeable", "operation not permitted", "access is denied", "requires root privileges"} {
		if strings.Contains(output, message) {
			return true
		}
	}
	return false
}

func privilegedDependencyCommand(ctx context.Context, timeout time.Duration, d dependency) error {
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return errors.New("sudo not found; update the tool with its installer or package manager")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sudo, append([]string{"--", d.path}, d.updateArgs...)...)
	// Keep the foreground terminal for sudo's own password prompt. Passwords
	// never pass through ytgo's buffers, flags, or environment variables.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func installedDependencies(ctx context.Context, c config, l *log.Logger) ([]dependency, error) {
	runtime, runtimePath := "deno", "deno"
	if c.jsRuntimes != "" {
		runtime, runtimePath, _ = strings.Cut(c.jsRuntimes, ":")
		if runtimePath == "" {
			runtimePath = runtime
		}
	}
	deps := []dependency{
		{name: "yt-dlp", path: "yt-dlp", versionArgs: []string{"--ignore-config", "--version"}, updateArgs: []string{"--ignore-config", "--update-to", c.updateChannel}},
		{name: "ffmpeg", path: "ffmpeg", versionArgs: []string{"-version"}},
		{name: runtime, path: expandHome(runtimePath), versionArgs: []string{"--version"}},
	}
	if runtime == "deno" {
		deps[2].updateArgs = []string{"upgrade"}
	}
	for i := range deps {
		d := &deps[i]
		path, err := exec.LookPath(d.path)
		if err != nil {
			return nil, fmt.Errorf("%s not found: %s", d.name, d.path)
		}
		d.path, err = filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		version, err := dependencyCommand(ctx, 10*time.Second, d.path, d.versionArgs...)
		if err != nil {
			return nil, fmt.Errorf("check %s version: %w: %s", d.name, err, version)
		}
		d.version = strings.SplitN(version, "\n", 2)[0]
		if d.version == "" {
			return nil, fmt.Errorf("%s returned an empty version", d.name)
		}
		l.Printf("%s: %s (%s)", d.name, d.version, d.path)
	}
	return deps, nil
}

// Serialize updates across processes sharing the cache, including the cache read.
func lockUpdateCache(ctx context.Context, path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func prepareDependencies(ctx context.Context, c config, l *log.Logger) error {
	deps, err := installedDependencies(ctx, c, l)
	if err != nil {
		return err
	}
	l.Print("ffmpeg updates are managed by Ubuntu; run make update-system")
	if len(deps[2].updateArgs) == 0 {
		l.Printf("%s updates are managed externally; automatic runtime updates apply to Deno", deps[2].name)
	}
	if c.updates == "off" && !c.updateOnly {
		return nil
	}
	cachePath := expandHome(c.updateCache)
	if cachePath == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return updateFailure(c, l, err)
		}
		cachePath = filepath.Join(dir, "ytgo", "updates.json")
	}
	// Bound waiting for another updater as well as individual update commands.
	lockCtx, cancel := context.WithTimeout(ctx, c.updateTimeout)
	lock, err := lockUpdateCache(lockCtx, cachePath)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return updateFailure(c, l, fmt.Errorf("lock update cache: %w", err))
	}
	defer lock.Close()
	records := map[string]updateRecord{}
	data, err := os.ReadFile(cachePath)
	if err == nil {
		if err := json.Unmarshal(data, &records); err != nil {
			l.Printf("warning: invalid update cache; checking again: %v", err)
			records = map[string]updateRecord{}
		}
		if records == nil {
			records = map[string]updateRecord{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		l.Printf("warning: cannot read update cache: %v", err)
	}
	var failures []error
	for _, d := range deps {
		if len(d.updateArgs) == 0 {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Another process may have upgraded this binary while we waited for the lock.
		version, versionErr := dependencyCommand(ctx, 10*time.Second, d.path, d.versionArgs...)
		if versionErr != nil {
			return fmt.Errorf("recheck %s before updating: %w", d.name, versionErr)
		}
		d.version = strings.SplitN(version, "\n", 2)[0]
		if d.version == "" {
			return fmt.Errorf("%s returned an empty version", d.name)
		}
		key := d.path + "|" + strings.Join(d.updateArgs, " ")
		record := records[key]
		age := time.Since(record.Checked)
		if c.updates == "daily" && !c.updateOnly && record.Version == d.version && age >= 0 && age < 24*time.Hour {
			l.Printf("%s: successful update check cached within 24 hours", d.name)
			continue
		}
		l.Printf("checking/updating %s...", d.name)
		output, err := dependencyCommand(ctx, c.updateTimeout, d.path, d.updateArgs...)
		if err != nil && ctx.Err() == nil && c.updateSudo && permissionFailure(output) {
			l.Printf("%s needs administrator permissions; retrying with sudo (password may be requested)", d.name)
			err = privilegedDependencyCommand(ctx, c.updateTimeout, d)
			output = "sudo updater output was shown in the terminal"
		}
		if err == nil {
			var version string
			version, err = dependencyCommand(ctx, 10*time.Second, d.path, d.versionArgs...)
			d.version = strings.SplitN(version, "\n", 2)[0]
			if err == nil && d.version == "" {
				err = errors.New("empty version after update")
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			delete(records, key)
			failure := fmt.Errorf("%s update failed: %w: %s; use its installer/package manager or administrator privileges if required", d.name, err, output)
			l.Printf("warning: %v", failure)
			failures = append(failures, failure)
			continue
		}
		records[key] = updateRecord{Version: d.version, Checked: time.Now().UTC()}
		l.Printf("%s update check succeeded: %s", d.name, d.version)
	}
	if err := saveUpdateRecords(cachePath, records); err != nil {
		l.Printf("warning: save update cache: %v", err)
		failures = append(failures, err)
	}
	if len(failures) > 0 {
		return updateFailure(c, l, errors.Join(failures...))
	}
	return nil
}

func updateFailure(c config, l *log.Logger, err error) error {
	if c.updateOnly {
		return err
	}
	l.Printf("warning: dependency freshness could not be verified; continuing with installed tools: %v", err)
	return nil
}

func saveUpdateRecords(path string, records map[string]updateRecord) error {
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".updates-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
