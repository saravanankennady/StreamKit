package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"elnova-streamkit.local/egress/confine"
	"elnova-streamkit.local/ingress/source"
)

const (
	videoBitrate   = 3_000_000
	audioBitrate   = 128_000
	frameWidth     = 1280
	frameHeight    = 720
	frameRate      = 30
	keyframeMs     = 2000
	timesliceMs    = 1000
	aacBitrate     = "128k"
	streamQueue    = 120 // chunks, so about two minutes of a stalled destination
	finalizeWindow = 24 * time.Hour
	stderrLines    = 12
)

// aacArgs encode AAC. MediaRecorder rounds audio timestamps to the millisecond, so they wobble against the 20 ms frames.
// aresample lays them out evenly, which the AAC encoder wants, without moving audio against video.
var aacArgs = []string{"-af", "aresample=async=1", "-c:a", "aac", "-b:a", aacBitrate, "-ar", "48000"}

// captureHasH264 says what MediaRecorder produced, from the MIME type the page reports.
func captureHasH264(mime string) bool {
	m := strings.ToLower(mime)
	return strings.Contains(m, "avc1") || strings.Contains(m, "h264")
}

// captureExtension names the capture file after its container.
func captureExtension(mime string) string {
	if strings.Contains(strings.ToLower(mime), "matroska") {
		return ".mkv"
	}
	return ".webm"
}

// mimeTypesFor chooses what to ask MediaRecorder for, best first. Chrome's own H.264 encoder means MP4 and RTMP need no
// second video encode. VP8 is the fallback for browsers built without it, and what WebM output needs.
func mimeTypesFor(jobType, format string) []string {
	switch jobType {
	case "audio_only":
		return []string{"audio/webm;codecs=opus"}
	case "video_only":
		if format == "webm" {
			return []string{"video/webm;codecs=vp8"}
		}
		return []string{"video/x-matroska;codecs=avc1", "video/webm;codecs=vp8"}
	}
	if format == "webm" {
		return []string{"video/webm;codecs=vp8,opus"}
	}
	return []string{"video/x-matroska;codecs=avc1,opus", "video/webm;codecs=vp8,opus"}
}

// finalizeArgs turns a finished capture into the delivered file. For MP4 it copies H.264 and encodes AAC, and for WebM
// it copies everything: MediaRecorder's stream has no index, so even a copy is what makes the result seekable.
func finalizeArgs(capture, output, format, mime string, audio, video bool) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-fflags", "+genpts", "-i", capture}
	switch format {
	case "webm":
		args = append(args, "-c", "copy")
	default:
		if video {
			if captureHasH264(mime) {
				args = append(args, "-c:v", "copy")
			} else {
				args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p")
			}
		}
		if audio {
			args = append(args, aacArgs...)
		}
		args = append(args, "-movflags", "+faststart")
	}
	return append(args, output)
}

// streamArgs pushes a live capture to one RTMP destination. RTMP carries only H.264 and AAC. target is a relay on this
// machine, so it holds no key, and tcURL is the address for the far server to see. See source.Relay.
func streamArgs(mime, target, tcURL string, audio, video bool) []string {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-fflags", "+genpts",
		"-probesize", "1M", "-analyzeduration", "1M", "-i", "pipe:0",
		"-protocol_whitelist", "rtmp,tcp", "-rtmp_tcurl", tcURL,
	}
	if video {
		if captureHasH264(mime) {
			args = append(args, "-c:v", "copy")
		} else {
			args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency", "-pix_fmt", "yuv420p",
				"-g", strconv.Itoa(frameRate*keyframeMs/1000), "-b:v", strconv.Itoa(videoBitrate))
		}
	} else {
		args = append(args, "-vn")
	}
	if audio {
		args = append(args, aacArgs...)
	} else {
		args = append(args, "-an")
	}
	return append(args, "-f", "flv", "-flvflags", "no_duration_filesize", target)
}

// tailBuffer keeps the end of a process's output, for error messages.
type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if len(t.data) > 16<<10 {
		t.data = append(t.data[:0], t.data[len(t.data)-(8<<10):]...)
	}
	return len(p), nil
}

// tail returns the last few lines, on one line.
func (t *tailBuffer) tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.data)), "\n")
	if len(lines) > stderrLines {
		lines = lines[len(lines)-stderrLines:]
	}
	return strings.Join(lines, " | ")
}

// mediaEnv is all a media process gets to see. Nothing from this worker's environment, like its database address or
// master key, belongs in a process parsing media from strangers.
func mediaEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "LANG=C.UTF-8"}
}

// runFFmpeg runs one ffmpeg to completion, with access to one directory, and no way to connect to anything.
func runFFmpeg(ctx context.Context, binary string, args []string, dir string, meter *cpuMeter) error {
	confined, confinedArgs := confine.Command(binary, args, confine.Spec{Write: []string{dir}})
	cmd := exec.CommandContext(ctx, confined, confinedArgs...)
	cmd.Env = mediaEnv()
	cmd.SysProcAttr = childAttrs()
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start ffmpeg: %w", err)
	}
	if meter != nil {
		meter.add(cmd.Process.Pid)
	}
	if err := cmd.Wait(); err != nil {
		if tail := stderr.tail(); tail != "" {
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	return nil
}

// probeDuration asks ffprobe how long a file is.
func probeDuration(ctx context.Context, ffprobe, path string) (time.Duration, error) {
	binary, args := confine.Command(ffprobe, []string{"-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", path}, confine.Spec{Read: []string{path}})
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = mediaEnv()
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	seconds, err := strconv.ParseFloat(string(bytes.TrimSpace(out)), 64)
	if err != nil {
		return 0, fmt.Errorf("ffprobe found no duration: %w", err)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// streamOutput is one RTMP destination. Chunks reach it through a bounded queue, so a slow destination cannot hold up
// the recording, and gets dropped if it falls too far behind.
type streamOutput struct {
	redacted string
	relay    *source.Relay
	cmd      *exec.Cmd
	queue    chan []byte
	stderr   tailBuffer
	done     chan struct{}

	mu      sync.Mutex
	err     error
	stopped bool
}

// startStream launches ffmpeg for a destination.
func startStream(ffmpeg, mime string, relay *source.Relay, audio, video bool, redacted string, meter *cpuMeter) (*streamOutput, error) {
	args := streamArgs(mime, relay.URL(), relay.TCURL(), audio, video)
	binary, confined := confine.Command(ffmpeg, args, confine.Spec{Connect: []int{relay.Port()}})
	cmd := exec.Command(binary, confined...)
	cmd.Env = mediaEnv()
	cmd.SysProcAttr = childAttrs()
	s := &streamOutput{redacted: redacted, relay: relay, cmd: cmd, queue: make(chan []byte, streamQueue), done: make(chan struct{})}
	cmd.Stderr = &s.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start ffmpeg: %w", err)
	}
	meter.add(cmd.Process.Pid)
	go func() {
		defer func() { _ = stdin.Close() }()
		for chunk := range s.queue {
			if _, err := stdin.Write(chunk); err != nil {
				for range s.queue { // Let senders finish. The exit is reported below.
				}
				return
			}
		}
	}()
	go func() {
		err := cmd.Wait()
		relay.Close()
		s.mu.Lock()
		if !s.stopped {
			// What the far server says can have the key in it: it is the name it knows the stream by.
			if why := relay.Err(); why != nil {
				err = why
			} else if tail := s.stderr.tail(); tail != "" {
				err = fmt.Errorf("%v: %s", err, tail)
			}
			if secret := relay.Secret(); secret != "" && err != nil {
				err = errors.New(strings.ReplaceAll(err.Error(), secret, "{key}"))
			}
			s.err = err
		}
		s.mu.Unlock()
		close(s.done)
	}()
	return s, nil
}

// send queues a chunk. It reports false if this destination has fallen too far behind.
func (s *streamOutput) send(chunk []byte) bool {
	select {
	case <-s.done:
		return true // Its exit is reported through failure.
	default:
	}
	select {
	case s.queue <- chunk:
		return true
	default:
		return false
	}
}

// failure returns why this output ended by itself, if it has.
func (s *streamOutput) failure() (error, bool) {
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.err == nil && !s.stopped {
			return errors.New("ffmpeg exited"), true
		}
		return s.err, s.err != nil
	default:
		return nil, false
	}
}

// finish ends the stream cleanly, or kills it if it will not end.
func (s *streamOutput) finish(wait time.Duration) {
	s.mu.Lock()
	first := !s.stopped
	s.stopped = true
	s.mu.Unlock()
	if first {
		close(s.queue)
	}
	select {
	case <-s.done:
	case <-time.After(wait):
		s.kill()
		<-s.done
	}
}

func (s *streamOutput) kill() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}
