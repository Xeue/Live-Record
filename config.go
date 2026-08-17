package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FeedConfig describes one SRT source we dial out to.
//
// URL is what the operator actually types or pastes — the same string their
// encoder shows. Host/Port/StreamID/Passphrase/Latency are derived from it, so
// nothing has to be picked apart by hand.
type FeedConfig struct {
	Name string `json:"name"` // used in the output filename
	URL  string `json:"url"`  // srt://host:port?latency=..&streamid=..&passphrase=..

	// Derived from URL by normalise(); persisted so the file stays readable.
	Host       string `json:"host"`
	Port       int    `json:"port"`
	StreamID   string `json:"streamId"`
	Passphrase string `json:"passphrase"`
	Latency    int    `json:"latency"`
}

// Config is the whole app config, read from and written back to config.json.
type Config struct {
	ListenPort int    `json:"listenPort"`
	OutputDir  string `json:"outputDir"`

	// FilePattern names the recordings. Tokens: {name} {date} {time} {datetime}
	FilePattern string `json:"filePattern"`

	// What each feed writes. Both can be on at once, from one SRT connection:
	// the ProRes master is what you finish from, the MXF is what you cut against
	// while it is still recording.
	//
	// Measured in Premiere Pro 26: a growing AVC-Intra MXF is treated as growing
	// media — the clip goes italic and self-updates on the refresh timer. A
	// growing ProRes QuickTime is not, because the QuickTime importer does not
	// declare growing-media support, so no amount of preference tuning makes it
	// refresh on its own.
	RecordProRes   bool `json:"recordProRes"`
	RecordAVCIntra bool `json:"recordAvcIntra"`

	// AVC-Intra class: 50, 100 or 200. 100 is 10-bit 4:2:2 at ~100 Mb/s.
	AVCIntraClass int `json:"avcIntraClass"`

	// ProRes variant: proxy | lt | standard | hq | 4444 | 4444xq
	ProResVariant string `json:"proresVariant"`

	// Hours of index space reserved up front in the moov header. A recording
	// that runs past this stops cleanly rather than corrupting.
	MaxHours int `json:"maxHours"`

	// Bytes of moov header reserved per second per track. 550 is GStreamer's
	// default; 700 leaves headroom at 50fps.
	ReservedBytesPerSec int `json:"reservedBytesPerSec"`

	// How often the moov index is rewritten, in seconds. This is the
	// granularity at which Premiere sees the file grow.
	MoovUpdateSec int `json:"moovUpdateSec"`

	// ReservedPrefill changes where the index lives while recording, and it is
	// worth testing against your NLE because the two layouts look very
	// different to a reader.
	//
	//   false (default) — the muxer keeps two index slots and alternates
	//     between them, flipping a free atom's size to swap which is live.
	//     Measured: on alternate polls the header is not at the front of the
	//     file at all, it is inside a large free block. A reader that walks the
	//     whole box tree copes; one that remembers where the header was does
	//     not. The advertised duration is always the real recorded duration.
	//
	//   true — the index is written once at a fixed offset and updated in
	//     place, which is the layout conventional recorders produce. The cost
	//     is that the file advertises the WHOLE reserved duration from the
	//     first frame rather than what has actually been recorded, so a reader
	//     that trusts it sees a full-length clip that is mostly empty.
	ReservedPrefill bool `json:"reservedPrefill"`

	// PreviewMode selects how you watch the feeds:
	//
	//   native  a GPU-rendered window per feed, straight off the decoder at
	//           full resolution and full frame rate. No scaling, no JPEG, no
	//           extra encode — the cheapest possible preview.
	//   browser a scaled MJPEG stream in the UI. Costs a scale + JPEG encode
	//           per feed, but works from another machine on the network.
	//   both    both of the above.
	PreviewMode string `json:"previewMode"`

	// Only used when PreviewMode includes "browser".
	PreviewWidth   int `json:"previewWidth"`
	PreviewHeight  int `json:"previewHeight"`
	PreviewFPS     int `json:"previewFps"`
	PreviewQuality int `json:"previewQuality"`

	Feeds []FeedConfig `json:"feeds"`
}

func defaultConfig() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		ListenPort:          7777,
		OutputDir:           filepath.Join(home, "Movies", "Live Record"),
		FilePattern:         "{name}_{date}_{time}",
		RecordProRes:        true,
		RecordAVCIntra:      false,
		AVCIntraClass:       100,
		ProResVariant:       "hq",
		MaxHours:            6,
		ReservedBytesPerSec: 700,
		MoovUpdateSec:       1,
		PreviewMode:         "native",
		PreviewWidth:        640,
		PreviewHeight:       360,
		PreviewFPS:          12,
		PreviewQuality:      70,
		Feeds: []FeedConfig{
			{Name: "CAM-A", URL: "srt://127.0.0.1:9001?latency=200"},
			{Name: "CAM-B", URL: "srt://127.0.0.1:9002?latency=200"},
		},
	}
}

var ProResVariants = []string{"proxy", "lt", "standard", "hq", "4444", "4444xq"}

var PreviewModes = []string{"native", "browser", "both"}

func (c *Config) nativePreview() bool {
	return c.PreviewMode == "native" || c.PreviewMode == "both"
}

func (c *Config) browserPreview() bool {
	return c.PreviewMode == "browser" || c.PreviewMode == "both"
}

func validVariant(v string) bool {
	for _, x := range ProResVariants {
		if x == v {
			return true
		}
	}
	return false
}

func loadConfig(path string) (*Config, error) {
	cfg := defaultConfig()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// First run: write the defaults out so there is something to edit.
		if err := cfg.normalise(); err != nil {
			return nil, err
		}
		if err := cfg.save(path); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, cfg.normalise()
}

// parseSRTURL pulls the connection details out of an srt:// URL. Anything the
// URL does not specify keeps whatever the feed already had.
func parseSRTURL(raw string, f *FeedConfig) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("SRT URL is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "srt://" + raw // tolerate a bare host:port
	}
	// Split the query off by hand before url.Parse. Every modern encoder emits
	// SRT access control as streamid=#!::r=live/feed,m=publish, and url.Parse
	// treats everything from the first '#' as a fragment — so the stream ID is
	// silently dropped and the connection is refused by the sender.
	base, query := raw, ""
	if i := strings.Index(raw, "?"); i >= 0 {
		base, query = raw[:i], raw[i+1:]
	}
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("not a valid SRT URL: %w", err)
	}
	if u.Scheme != "srt" {
		return fmt.Errorf("URL must start with srt:// (got %q)", u.Scheme)
	}
	host, portStr := u.Hostname(), u.Port()
	if host == "" {
		return fmt.Errorf("no host in %q", raw)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("no valid port in %q", raw)
	}
	f.Host, f.Port = host, port

	// Parse the query manually for the same reason it was split off: values may
	// legitimately contain '#', ':' and ','.
	q := map[string]string{}
	for _, pair := range strings.Split(query, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		if dec, err := url.QueryUnescape(v); err == nil {
			v = dec
		}
		q[strings.ToLower(strings.TrimSpace(k))] = v
	}
	// An explicitly empty value clears the stored one; an absent key keeps it.
	if v, ok := q["streamid"]; ok {
		f.StreamID = v
	}
	if v, ok := q["passphrase"]; ok {
		f.Passphrase = v
	}
	if v := q["latency"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Latency = n
		}
	}
	if f.Latency <= 0 {
		f.Latency = 200
	}
	return nil
}

// normalise validates the config and fills in everything derived.
func (c *Config) normalise() error {
	c.ProResVariant = strings.ToLower(strings.TrimSpace(c.ProResVariant))
	if !validVariant(c.ProResVariant) {
		return fmt.Errorf("ProRes variant %q must be one of: %s",
			c.ProResVariant, strings.Join(ProResVariants, ", "))
	}
	if strings.TrimSpace(c.OutputDir) == "" {
		return fmt.Errorf("output folder is empty")
	}
	if c.FilePattern == "" {
		c.FilePattern = "{name}_{date}_{time}"
	}
	if !strings.Contains(c.FilePattern, "{name}") && len(c.Feeds) > 1 {
		return fmt.Errorf("file pattern must contain {name} when there is more than one feed, " +
			"or every feed would write to the same file")
	}
	if c.ListenPort <= 0 || c.ListenPort > 65535 {
		c.ListenPort = 7777
	}
	if len(c.Feeds) == 0 {
		return fmt.Errorf("no feeds configured")
	}
	if c.MaxHours < 1 {
		c.MaxHours = 1
	}
	if c.MoovUpdateSec < 1 {
		c.MoovUpdateSec = 1
	}
	if c.ReservedBytesPerSec < 100 {
		c.ReservedBytesPerSec = 700
	}
	if c.PreviewFPS < 1 {
		c.PreviewFPS = 12
	}
	if c.PreviewWidth < 64 {
		c.PreviewWidth = 640
	}
	if c.PreviewHeight < 64 {
		c.PreviewHeight = 360
	}
	if c.PreviewQuality < 10 || c.PreviewQuality > 100 {
		c.PreviewQuality = 70
	}
	if !c.RecordProRes && !c.RecordAVCIntra {
		return fmt.Errorf("nothing would be recorded — enable ProRes, AVC-Intra, or both")
	}
	switch c.AVCIntraClass {
	case 50, 100, 200:
	default:
		c.AVCIntraClass = 100
	}
	c.PreviewMode = strings.ToLower(strings.TrimSpace(c.PreviewMode))
	if !c.nativePreview() && !c.browserPreview() {
		c.PreviewMode = "native"
	}

	seen := map[string]bool{}
	for i := range c.Feeds {
		f := &c.Feeds[i]
		if f.Name == "" {
			f.Name = fmt.Sprintf("FEED-%d", i+1)
		}
		// The name becomes part of a filename, so keep it filesystem-safe.
		f.Name = sanitiseName(f.Name)
		// Compared case-insensitively because feeds are looked up that way;
		// otherwise "cam-a" and "CAM-A" both validate and every button press
		// for the second one operates the first.
		key := strings.ToLower(f.Name)
		if seen[key] {
			return fmt.Errorf("two feeds are both named %q — names must be unique", f.Name)
		}
		seen[key] = true

		// Rebuild URL from parts if only the parts were supplied (older config).
		if strings.TrimSpace(f.URL) == "" && f.Host != "" && f.Port > 0 {
			f.URL = fmt.Sprintf("srt://%s:%d?latency=%d", f.Host, f.Port, max(f.Latency, 200))
		}
		if err := parseSRTURL(f.URL, f); err != nil {
			return fmt.Errorf("feed %s: %w", f.Name, err)
		}
	}
	return nil
}

// resolveFilename expands the file pattern for one feed at one moment.
func (c *Config) resolveFilename(feedName string, t time.Time) string {
	r := strings.NewReplacer(
		"{name}", feedName,
		"{date}", t.Format("2006-01-02"),
		"{time}", t.Format("150405"),
		"{datetime}", t.Format("2006-01-02_150405"),
	)
	out := sanitisePath(r.Replace(c.FilePattern))
	if out == "" {
		out = feedName + "_" + t.Format("2006-01-02_150405")
	}
	return out
}

func (c *Config) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func (c *Config) clone() *Config {
	out := *c
	out.Feeds = append([]FeedConfig(nil), c.Feeds...)
	return &out
}

func sanitiseName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
	if s == "" {
		s = "FEED"
	}
	return s
}

// sanitisePath keeps a resolved filename to one path segment: a pattern must
// not be able to write outside the chosen output folder.
func sanitisePath(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', 0:
			return '-'
		}
		return r
	}, s)
	return strings.TrimLeft(s, ".")
}
