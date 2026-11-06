package ingress

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const (
	// Browsers must decode this, so: H.264 constrained baseline without B-frames, and Opus.
	maxWidth       = 1920
	outputFPS      = 30
	videoKbps      = 2500
	audioKbps      = 96
	keyframeSecond = 2
	rtpPacketSize  = 1200 // Leaves room for SRTP and the tunnels a path may add, under a 1500-byte MTU.
)

// outputArgs is everything after the input: one transcode, and one RTP stream each for video and audio. The tee muxer is
// what lets a source have only one of the two: the slave with no stream fails to open, and onfail=ignore carries on.
func outputArgs(videoPort, audioPort int) []string {
	rtp := func(port int) string {
		return fmt.Sprintf("rtp://127.0.0.1:%d?pkt_size=%d&rtcpport=%d", port, rtpPacketSize, port)
	}
	return []string{
		"-map", "0:v:0?", "-map", "0:a:0?",
		"-vf", fmt.Sprintf("scale='min(%d,iw)':-2,fps=%d,format=yuv420p", maxWidth, outputFPS),
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency", "-profile:v", "baseline", "-bf", "0",
		"-b:v", fmt.Sprintf("%dk", videoKbps), "-maxrate", fmt.Sprintf("%dk", videoKbps*5/4), "-bufsize", fmt.Sprintf("%dk", videoKbps),
		"-g", strconv.Itoa(outputFPS * keyframeSecond), "-keyint_min", strconv.Itoa(outputFPS * keyframeSecond), "-sc_threshold", "0",
		// A subscriber can join at any keyframe, so each must carry what is needed to decode it.
		"-x264-params", "repeat-headers=1",
		"-c:a", "libopus", "-b:a", fmt.Sprintf("%dk", audioKbps), "-ar", "48000", "-ac", "2", "-frame_duration", "20",
		"-f", "tee", fmt.Sprintf("[select=v:f=rtp:onfail=ignore]%s|[select=a:f=rtp:onfail=ignore]%s", rtp(videoPort), rtp(audioPort)),
	}
}

func baseArgs() []string {
	return []string{"-hide_banner", "-loglevel", "info", "-nostats", "-nostdin", "-progress", "pipe:1"}
}

// rtmpArgs reads an FLV stream from stdin.
func rtmpArgs(videoPort, audioPort int) []string {
	args := append(baseArgs(), "-f", "flv", "-probesize", "2000000", "-analyzeduration", "1000000", "-i", "pipe:0")
	return append(args, outputArgs(videoPort, audioPort)...)
}

// allowedFormats are the containers a URL input can hold. Without a list, ffmpeg goes by what the data looks like, and some
// formats open other addresses in ways that do not go through the proxy: a concat list served from a good address names
// further files, which ffmpeg then fetches directly. The segments of an HLS playlist are what it has mov and mpegts for.
const allowedFormats = "hls,mpegts,mov,mp4,m4a,3gp,3g2,mj2,flv,live_flv,matroska,webm,mp3,aac,ogg,wav,asf,avi,mpeg,h264,hevc"

// urlArgs pulls from a URL. Files have to be paced to real time, and live sources already are. Each scheme may use only
// the protocols it needs, so a playlist cannot point ffmpeg at a local file, and HTTP goes through the proxy that checks
// every connection it makes. RTMP goes through a relay, and tcURL is the real server's address for it to see.
func urlArgs(u *url.URL, proxyURL, tcURL string, videoPort, audioPort int) []string {
	args := baseArgs()
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		args = append(args, "-protocol_whitelist", "http,https,tcp,tls,crypto,httpproxy", "-http_proxy", proxyURL, "-re", "-rw_timeout", "15000000")
	case "rtmp":
		args = append(args, "-protocol_whitelist", "rtmp,tcp")
		if tcURL != "" {
			args = append(args, "-rtmp_tcurl", tcURL)
		}
	case "srt":
		args = append(args, "-protocol_whitelist", "srt,udp")
	}
	args = append(args, "-format_whitelist", allowedFormats, "-i", u.String())
	return append(args, outputArgs(videoPort, audioPort)...)
}

var (
	inputStreamLine = regexp.MustCompile(`^\s*Stream #0:\d+.*?: (Video|Audio): (\w+)`)
	videoSize       = regexp.MustCompile(`, (\d{2,5})x(\d{2,5})`)
	videoRate       = regexp.MustCompile(`, ([\d.]+) fps`)
)

// inputInfo is what ffmpeg found in the input.
type inputInfo struct {
	mu         sync.Mutex
	width      int
	height     int
	fps        float64
	videoCodec string
	audioCodec string
	done       bool
}

// parseLine reads ffmpeg's description of its input, which ends where it starts describing the output.
func (i *inputInfo) parseLine(line string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.done {
		return
	}
	if strings.HasPrefix(line, "Stream mapping") || strings.HasPrefix(line, "Output #") {
		i.done = true
		return
	}
	m := inputStreamLine.FindStringSubmatch(line)
	if m == nil {
		return
	}
	switch m[1] {
	case "Video":
		i.videoCodec = m[2]
		if s := videoSize.FindStringSubmatch(line); s != nil {
			i.width, _ = strconv.Atoi(s[1])
			i.height, _ = strconv.Atoi(s[2])
		}
		if r := videoRate.FindStringSubmatch(line); r != nil {
			i.fps, _ = strconv.ParseFloat(r[1], 64)
		}
	case "Audio":
		i.audioCodec = m[2]
	}
}

// snapshot copies what is known so far.
func (i *inputInfo) snapshot() (width, height int, fps float64, video, audio string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.width, i.height, i.fps, i.videoCodec, i.audioCodec
}

// progress reads ffmpeg's -progress output for the frame count.
type progress struct {
	mu     sync.Mutex
	frames int64
}

func (p *progress) parseLine(line string) {
	key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
	if !ok || key != "frame" {
		return
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
		p.mu.Lock()
		p.frames = n
		p.mu.Unlock()
	}
}

func (p *progress) frameCount() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.frames
}
