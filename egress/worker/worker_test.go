package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"elnova-streamkit.local/egress/confine"
)

func TestMain(m *testing.M) {
	confine.RunHelperIfAsked()
	os.Exit(m.Run())
}

func TestMimeTypesPreferH264ForMP4(t *testing.T) {
	if got := mimeTypesFor("room_composite", "mp4"); !captureHasH264(got[0]) || captureHasH264(got[len(got)-1]) {
		t.Errorf("MP4 should try H.264 and fall back to VP8: %v", got)
	}
	if got := mimeTypesFor("room_composite", ""); !captureHasH264(got[0]) {
		t.Errorf("a stream needs H.264: %v", got)
	}
	if got := mimeTypesFor("room_composite", "webm"); captureHasH264(got[0]) {
		t.Errorf("WebM cannot hold H.264: %v", got)
	}
	for _, m := range mimeTypesFor("audio_only", "mp4") {
		if !strings.HasPrefix(m, "audio/") {
			t.Errorf("audio only: %v", m)
		}
	}
	for _, m := range mimeTypesFor("video_only", "webm") {
		if strings.Contains(m, "opus") {
			t.Errorf("video only should not record audio: %v", m)
		}
	}
}

func TestCaptureExtensionFollowsTheContainer(t *testing.T) {
	if captureExtension("video/x-matroska;codecs=avc1,opus") != ".mkv" || captureExtension("video/webm;codecs=vp8") != ".webm" {
		t.Fatal("wrong extension")
	}
}

func hasSeq(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestFinalizeArgs(t *testing.T) {
	h264 := "video/x-matroska;codecs=avc1,opus"
	vp8 := "video/webm;codecs=vp8,opus"

	args := finalizeArgs("c.mkv", "o.mp4", "mp4", h264, true, true)
	if !hasSeq(args, "-c:v", "copy") || !hasSeq(args, aacArgs...) || !hasSeq(args, "-movflags", "+faststart") {
		t.Errorf("H.264 to MP4 should copy video and encode AAC: %v", args)
	}
	args = finalizeArgs("c.webm", "o.mp4", "mp4", vp8, true, true)
	if !hasSeq(args, "-c:v", "libx264", "-preset", "veryfast") {
		t.Errorf("VP8 to MP4 has to transcode: %v", args)
	}
	args = finalizeArgs("c.webm", "o.webm", "webm", vp8, true, true)
	if !hasSeq(args, "-c", "copy") {
		t.Errorf("WebM to WebM should copy: %v", args)
	}
	args = finalizeArgs("c.webm", "o.mp4", "mp4", "audio/webm;codecs=opus", true, false)
	if slices.Contains(args, "-c:v") {
		t.Errorf("audio only has no video to handle: %v", args)
	}
	args = finalizeArgs("c.mkv", "o.mp4", "mp4", "video/x-matroska;codecs=avc1", false, true)
	if slices.Contains(args, "-c:a") {
		t.Errorf("video only has no audio to handle: %v", args)
	}
	if args[len(args)-1] != "o.mp4" {
		t.Errorf("the output goes last: %v", args)
	}
}

func TestStreamArgs(t *testing.T) {
	args := streamArgs("video/x-matroska;codecs=avc1,opus", "rtmp://h/a/k", "rtmp://real/a", true, true)
	if !hasSeq(args, "-rtmp_tcurl", "rtmp://real/a") {
		t.Errorf("%v", args)
	}
	if !hasSeq(args, "-c:v", "copy") || !hasSeq(args, aacArgs...) || args[len(args)-1] != "rtmp://h/a/k" || !hasSeq(args, "-f", "flv") {
		t.Errorf("%v", args)
	}
	args = streamArgs("video/webm;codecs=vp8,opus", "rtmp://h/a/k", "rtmp://real/a", true, true)
	if !hasSeq(args, "-c:v", "libx264") {
		t.Errorf("RTMP carries only H.264: %v", args)
	}
	if args := streamArgs("audio/webm;codecs=opus", "rtmp://h/a/k", "rtmp://real/a", true, false); !slices.Contains(args, "-vn") {
		t.Errorf("%v", args)
	}
	if args := streamArgs("video/x-matroska;codecs=avc1", "rtmp://h/a/k", "rtmp://real/a", false, true); !slices.Contains(args, "-an") {
		t.Errorf("%v", args)
	}
}

func TestParseProcStat(t *testing.T) {
	// A process name can hold spaces and parentheses, so fields must be counted from the last parenthesis.
	line := "4242 (chrome (renderer) x) S 77 4242 4242 0 -1 4194304 1 0 0 0 150 50 0 0 20 0 1 0 1 1 1"
	info, ok := parseProcStat([]byte(line))
	if !ok || info.ppid != 77 || info.ticks != 200 {
		t.Fatalf("%+v %v", info, ok)
	}
	if _, ok := parseProcStat([]byte("garbage")); ok {
		t.Fatal("garbage parsed")
	}
	if _, ok := parseProcStat([]byte("1 (a) S 1 2")); ok {
		t.Fatal("a short line parsed")
	}
}

func TestDescendantsFollowsTheTree(t *testing.T) {
	procs := map[int]procInfo{
		1:  {ppid: 0},
		10: {ppid: 1},
		11: {ppid: 10},
		12: {ppid: 11},
		20: {ppid: 1},
		21: {ppid: 20},
	}
	got := descendants(procs, []int{10, 999})
	if len(got) != 3 || !got[10] || !got[11] || !got[12] {
		t.Fatalf("%v", got)
	}
}

func TestRedactKeysHidesStreamKeysInFFmpegOutput(t *testing.T) {
	got := redactKeys(`[rtmp @ 0x1] Cannot open connection 'rtmp://h.example/app/abcdefghijklmnop': Connection refused`)
	if strings.Contains(got, "defghijklm") {
		t.Fatalf("leaked: %s", got)
	}
	if !strings.Contains(got, "Connection refused") {
		t.Fatalf("lost the reason: %s", got)
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	var b tailBuffer
	for i := 0; i < 100; i++ {
		_, _ = b.Write([]byte("line " + strings.Repeat("x", 500) + "\n"))
	}
	_, _ = b.Write([]byte("the real error\n"))
	if !strings.HasSuffix(b.tail(), "the real error") {
		t.Fatalf("%q", b.tail()[len(b.tail())-40:])
	}
}

func TestCheckWorkDirNoticesAReadOnlyDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := checkWorkDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	if err := checkWorkDir(dir); err == nil {
		t.Fatal("a read-only directory passed the check")
	}
}

func TestCheckRunsNoticesABinaryThatFails(t *testing.T) {
	if err := checkRuns(context.Background(), "/bin/true"); err != nil {
		t.Fatal(err)
	}
	if err := checkRuns(context.Background(), "/bin/false"); err == nil {
		t.Fatal("a failing binary passed the check")
	}
	if err := checkRuns(context.Background(), "/does/not/exist"); err == nil {
		t.Fatal("a missing binary passed the check")
	}
}

func TestChromeReasonLeavesOutWhatItCanDoWithout(t *testing.T) {
	var b tailBuffer
	_, _ = b.Write([]byte("[1:2:ERROR:dbus/bus.cc:405] Failed to connect to the bus\nsandbox_linux: No usable sandbox!\n"))
	got := chromeReason(&b)
	if strings.Contains(got, "dbus") || !strings.Contains(got, "No usable sandbox") {
		t.Fatal(got)
	}
}

func TestChromeHasNoDisplayOrSoundDependencies(t *testing.T) {
	args := chromeArgs("/p", false)
	if !slices.Contains(args, "--headless=new") {
		t.Errorf("%v", args)
	}
	if slices.Contains(args, "--no-sandbox") {
		t.Errorf("the sandbox stays on unless asked: %v", args)
	}
	if !slices.Contains(chromeArgs("/p", true), "--no-sandbox") {
		t.Errorf("no way to turn the sandbox off")
	}
	if !slices.Contains(args, "--disable-audio-output") {
		t.Errorf("nothing about recording should depend on a sound card: %v", args)
	}
}

// finish runs finalizeArgs through a real ffmpeg and says what came out.
func finish(t *testing.T, input []string, format, mime string, audio, video bool) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "capture.webm")
	args := append([]string{"-hide_banner", "-v", "error", "-y"}, input...)
	if out, err := exec.Command("ffmpeg", append(args, "-t", "1", capture)...).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	output := filepath.Join(dir, "output."+format)
	if err := runFFmpeg(context.Background(), "ffmpeg", finalizeArgs(capture, output, format, mime, audio, video), dir, nil); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", output).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// Chrome built without an H.264 encoder records VP8, and the delivered MP4 still has to be H.264 and AAC.
func TestFinalizeTranscodesVP8ToH264(t *testing.T) {
	got := finish(t, []string{"-f", "lavfi", "-i", "testsrc2=s=160x90:r=10", "-f", "lavfi", "-i", "sine=r=48000", "-c:v", "libvpx", "-c:a", "libopus"},
		"mp4", "video/webm;codecs=vp8,opus", true, true)
	if got != "h264 aac" {
		t.Fatalf("got %q", got)
	}
}

func TestFinalizeCopiesWebM(t *testing.T) {
	got := finish(t, []string{"-f", "lavfi", "-i", "testsrc2=s=160x90:r=10", "-f", "lavfi", "-i", "sine=r=48000", "-c:v", "libvpx", "-c:a", "libopus"},
		"webm", "video/webm;codecs=vp8,opus", true, true)
	if got != "vp8 opus" {
		t.Fatalf("got %q", got)
	}
}

func TestFinalizeAudioOnlyToMP4(t *testing.T) {
	got := finish(t, []string{"-f", "lavfi", "-i", "sine=r=48000", "-c:a", "libopus"}, "mp4", "audio/webm;codecs=opus", true, false)
	if got != "aac" {
		t.Fatalf("got %q", got)
	}
}

func TestSalvageRetriesWaitLongerEachTime(t *testing.T) {
	now := time.Unix(1000, 0)
	var state salvageState
	var waits []time.Duration
	for range 10 {
		state = state.next(now)
		waits = append(waits, state.Next.Sub(now))
	}
	if waits[0] != salvageRetryFirst || waits[1] != 2*salvageRetryFirst || waits[9] != salvageRetryMost {
		t.Fatalf("%v", waits)
	}
}

func TestLockDirKeepsOutOtherHolders(t *testing.T) {
	dir := t.TempDir()
	release, ok := lockDir(dir)
	if !ok {
		t.Fatal("could not lock")
	}
	if _, ok := lockDir(dir); ok {
		t.Fatal("two holders")
	}
	release()
	release2, ok := lockDir(dir)
	if !ok {
		t.Fatal("not released")
	}
	release2()
}

func TestARelativeWorkDirBecomesAbsolute(t *testing.T) {
	dir := workDir("tests/e2e/.tmp/work")
	if !filepath.IsAbs(dir) {
		t.Fatalf("%s is relative, so the launcher's file:// address would name a host", dir)
	}
	if url, err := writeLauncher(t.TempDir(), "http://127.0.0.1:1/"); err != nil || !strings.HasPrefix(url, "file:///") {
		t.Fatalf("launcher address %q: %v", url, err)
	}
}

func TestLauncherHoldsTheTokenOutOfTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	url, err := writeLauncher(dir, "http://127.0.0.1:1/index.html#token=one-time")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(url, "one-time") {
		t.Fatalf("this goes on a command line: %s", url)
	}
	info, err := os.Stat(strings.TrimPrefix(url, "file://"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
}

func TestAudioWatchNoticesPacketsWithNoSamples(t *testing.T) {
	stuck := json.RawMessage(`[{"packets":300,"samples":0,"energy":0},{"packets":300,"samples":48000,"energy":1.5}]`)
	fine := json.RawMessage(`[{"packets":300,"samples":48000,"energy":0}]`)
	waiting := json.RawMessage(`[{"packets":0,"samples":0,"energy":0}]`)
	var w audioWatch
	for i := 1; i < audioStuckAfter; i++ {
		if w.note(stuck) {
			t.Fatalf("reported after %d reports", i)
		}
	}
	if w.note(fine) {
		t.Fatal("samples coming out is fine, even with no energy: a quiet room is not a fault")
	}
	for i := 1; i < audioStuckAfter; i++ {
		if w.note(stuck) {
			t.Fatal("the count should start again")
		}
	}
	if !w.note(stuck) {
		t.Fatal("this has gone on long enough to report")
	}
	if w.note(stuck) {
		t.Fatal("once is enough")
	}
	var quiet audioWatch
	for range 10 {
		if quiet.note(waiting) || quiet.note(nil) {
			t.Fatal("no packets yet is not this problem")
		}
	}
}
