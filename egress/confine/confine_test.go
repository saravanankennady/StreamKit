package confine

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	RunHelperIfAsked()
	if os.Getenv("CONFINE_TEST_PROTECT") == "1" {
		if err := Protect(); err != nil {
			os.Stdout.WriteString("protect failed: " + err.Error())
			os.Exit(3)
		}
		os.Stdout.WriteString("ready\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func needLandlock(t *testing.T, version int) {
	t.Helper()
	abi, err := landlockABI()
	if err != nil || abi < version {
		t.Skipf("this kernel has Landlock version %d, and this test needs %d", abi, version)
	}
}

// sh runs a shell command through the helper, and returns its exit code and output.
func sh(t *testing.T, spec Spec, script string) (int, string) {
	t.Helper()
	binary, args := Command("/bin/sh", []string{"-c", script}, spec)
	out, err := exec.Command(binary, args...).CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(out)
	}
	t.Fatal(err)
	return -1, ""
}

func TestLimitsFilesToWhatTheSpecAllows(t *testing.T) {
	needLandlock(t, 1)
	dir := t.TempDir()
	mine := filepath.Join(dir, "job")
	other := filepath.Join(dir, "other-job")
	for _, d := range []string{mine, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(other, "capture.webm")
	if err := os.WriteFile(secret, []byte("someone else's"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Write: []string{mine}}
	if code, out := sh(t, spec, "echo hello > "+mine+"/out && cat "+mine+"/out"); code != 0 || !strings.Contains(out, "hello") {
		t.Fatalf("its own directory: exit %d: %s", code, out)
	}
	for name, script := range map[string]string{
		"another job's file":    "cat " + secret,
		"writing there":         "echo x > " + other + "/new",
		"a parent's env":        "cat /proc/$PPID/environ",
		"any process's details": "cat /proc/1/cmdline",
		"a config directory":    "ls /etc/streamkit /etc/passwd",
		"the home directory":    "ls " + os.Getenv("HOME"),
	} {
		if code, out := sh(t, spec, script); code == 0 {
			t.Errorf("%s should be refused: %s", name, out)
		}
	}
}

func TestLimitsConnectionsToThePortsAllowed(t *testing.T) {
	needLandlock(t, 4)
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("needs bash")
	}
	l1 := listen(t)
	l2 := listen(t)
	connect := func(spec Spec, port int) int {
		code, _ := sh(t, spec, `exec bash -c 'exec 3<>/dev/tcp/127.0.0.1/`+strconv.Itoa(port)+`'`)
		return code
	}
	spec := Spec{Connect: []int{l1}, Read: []string{"/usr/bin"}}
	if got := connect(spec, l1); got != 0 {
		t.Errorf("the allowed port: exit %d", got)
	}
	if got := connect(spec, l2); got == 0 {
		t.Error("another port should be refused")
	}
	if got := connect(Spec{}, l1); got == 0 {
		t.Error("no ports allowed means no connections")
	}
	if got := connect(Spec{AnyNetwork: true}, l2); got != 0 {
		t.Errorf("AnyNetwork: exit %d", got)
	}
}

func TestProtectHidesTheEnvironmentFromTheSameUser(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "CONFINE_TEST_PROTECT=1", "SECRET_FOR_TEST=do-not-read-me")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	buf := make([]byte, 16)
	if _, err := stdout.Read(buf); err != nil {
		t.Fatal(err)
	}
	proc := filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid))
	for _, name := range []string{"environ", "mem", "maps"} {
		if data, err := os.ReadFile(filepath.Join(proc, name)); err == nil {
			t.Errorf("%s should not be readable, and gave %d bytes", name, len(data))
		}
	}
}

func TestReapPassWaitsAPassBeforeReaping(t *testing.T) {
	if got := deadChildren(os.Getpid()); len(got) != 0 {
		t.Fatalf("no dead children to start with: %v", got)
	}
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	var seen map[int]bool
	for range 100 {
		if seen = reapPass(nil); seen[pid] {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !seen[pid] {
		t.Fatal("the dead child was not noticed")
	}
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err != nil {
		t.Fatal("the first pass should leave it for os/exec to wait for")
	}
	reapPass(seen)
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err == nil {
		t.Fatal("the second pass should have reaped it")
	}
}

func listen(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}
