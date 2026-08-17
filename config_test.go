package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseSRTURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		host    string
		port    int
		stream  string
		pass    string
		latency int
		wantErr bool
	}{
		{name: "plain", url: "srt://10.0.0.5:9001", host: "10.0.0.5", port: 9001, latency: 200},
		{name: "with latency", url: "srt://10.0.0.5:9001?latency=800",
			host: "10.0.0.5", port: 9001, latency: 800},
		{name: "bare host:port is tolerated", url: "192.168.1.9:4001",
			host: "192.168.1.9", port: 4001, latency: 200},
		// The form every modern encoder emits. url.Parse would treat everything
		// from the '#' as a fragment and drop the stream ID entirely, so the
		// feed dials with no ID and the sender refuses it.
		{name: "SRT access control stream id",
			url:    "srt://host.example:9000?streamid=#!::r=live/feed,m=publish&latency=500",
			host:   "host.example", port: 9000, latency: 500,
			stream: "#!::r=live/feed,m=publish"},
		{name: "passphrase", url: "srt://h:9000?passphrase=s3cr3t!pass&latency=200",
			host: "h", port: 9000, pass: "s3cr3t!pass", latency: 200},
		{name: "no port", url: "srt://10.0.0.5", wantErr: true},
		{name: "wrong scheme", url: "rtmp://10.0.0.5:9001", wantErr: true},
		{name: "empty", url: "", wantErr: true},
		{name: "port out of range", url: "srt://10.0.0.5:99999", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var f FeedConfig
			err := parseSRTURL(tc.url, &f)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.url)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSRTURL(%q): %v", tc.url, err)
			}
			if f.Host != tc.host || f.Port != tc.port {
				t.Errorf("got %s:%d, want %s:%d", f.Host, f.Port, tc.host, tc.port)
			}
			if f.StreamID != tc.stream {
				t.Errorf("StreamID = %q, want %q", f.StreamID, tc.stream)
			}
			if f.Passphrase != tc.pass {
				t.Errorf("Passphrase = %q, want %q", f.Passphrase, tc.pass)
			}
			if f.Latency != tc.latency {
				t.Errorf("Latency = %d, want %d", f.Latency, tc.latency)
			}
		})
	}
}

// Two feeds differing only in case would both validate, but lookups are
// case-insensitive, so the second would be permanently uncontrollable.
func TestConfigRejectsNamesDifferingOnlyByCase(t *testing.T) {
	c := defaultConfig()
	c.Feeds = []FeedConfig{
		{Name: "CAM-A", URL: "srt://127.0.0.1:9001"},
		{Name: "cam-a", URL: "srt://127.0.0.1:9002"},
	}
	if err := c.normalise(); err == nil {
		t.Error("expected duplicate-name error for CAM-A vs cam-a")
	}
}

// A pattern must not be able to write outside the chosen output folder.
func TestResolveFilenameStaysInFolder(t *testing.T) {
	c := defaultConfig()
	at := time.Date(2026, 8, 14, 15, 4, 5, 0, time.UTC)
	for _, pattern := range []string{
		"../../etc/{name}", "/absolute/{name}", "{name}/../../escape", "..",
	} {
		c.FilePattern = pattern
		got := c.resolveFilename("CAM-A", at)
		if filepath.Base(got) != got {
			t.Errorf("pattern %q produced %q, which is not a single path segment", pattern, got)
		}
	}
}

func TestResolveFilenameTokens(t *testing.T) {
	c := defaultConfig()
	c.FilePattern = "{name}_{date}_{time}"
	at := time.Date(2026, 8, 14, 15, 4, 5, 0, time.UTC)
	if got, want := c.resolveFilename("CAM-A", at), "CAM-A_2026-08-14_150405"; got != want {
		t.Errorf("resolveFilename = %q, want %q", got, want)
	}
	c.FilePattern = "{datetime}-{name}"
	if got, want := c.resolveFilename("ISO1", at), "2026-08-14_150405-ISO1"; got != want {
		t.Errorf("resolveFilename = %q, want %q", got, want)
	}
}

// filesink truncates, so an existing take must never be chosen as the target.
func TestUniquePathNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "CAM-A_2026-08-14_150405.mov")

	if got := uniquePath(p); got != p {
		t.Errorf("unused name changed: got %q want %q", got, p)
	}
	if err := os.WriteFile(p, []byte("take one"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := uniquePath(p)
	if second == p {
		t.Fatal("returned a path that already exists — the previous take would be truncated")
	}
	if err := os.WriteFile(second, []byte("take two"), 0o644); err != nil {
		t.Fatal(err)
	}
	third := uniquePath(p)
	if third == p || third == second {
		t.Fatalf("collided again: %q", third)
	}
	// The original must be untouched.
	if b, _ := os.ReadFile(p); string(b) != "take one" {
		t.Errorf("original file was modified: %q", b)
	}
}
