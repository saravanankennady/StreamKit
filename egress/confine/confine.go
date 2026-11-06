// Package confine keeps what a media worker holds away from the media processes it starts. Chrome and ffmpeg parse media
// from strangers, and they run as the same user as the worker, which holds the database address and the master key.
//
// Two things stand between them. The worker makes itself undumpable, which is what the kernel looks at before it lets
// another process of the same user read its memory or its environment through /proc. And ffmpeg starts through a small
// helper, this same binary run again, which limits with Landlock which files it can touch and which TCP ports it can
// connect to, before it becomes ffmpeg. Chrome has its own sandbox for the parts that touch media.
//
// Neither stops a media process from sending signals to others of the same user. See docs/api/media-services.md.
package confine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// helperArg as the first argument makes a worker binary act as the helper instead.
const helperArg = "__streamkit-confined-exec"

// Spec says what a confined process may reach. Everything else is refused, and that includes all of /proc.
type Spec struct {
	// Read lists files and directories it may read, on top of what any program needs to start.
	Read []string `json:"read,omitempty"`
	// Write lists files and directories it may read and change.
	Write []string `json:"write,omitempty"`
	// Connect lists the TCP ports it may connect to. No ports means no TCP connections at all. UDP is not limited, because
	// Landlock cannot do that on the kernels this runs on.
	Connect []int `json:"connect,omitempty"`
	// AnyNetwork leaves network connections alone.
	AnyNetwork bool `json:"any_network,omitempty"`
}

// systemRead is what programs and the libraries they load need. None of it is ours, and nothing in it is secret. /etc is
// listed file by file, because a deployment's configuration can be in there.
var systemRead = []string{
	"/usr", "/lib", "/lib64", "/lib32", "/bin", "/sbin",
	"/etc/ld.so.cache", "/etc/alternatives", "/etc/ssl", "/etc/ca-certificates", "/etc/fonts", "/etc/localtime",
	"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/etc/host.conf", "/etc/gai.conf",
	"/sys/devices/system/cpu", "/dev/urandom", "/dev/random",
}

var systemWrite = []string{"/dev/null"}

// self is found early, because once a process is undumpable that gets harder, and the binary may be replaced later.
var self, _ = os.Executable()

// Command returns what to run to start binary with args under spec.
func Command(binary string, args []string, spec Spec) (string, []string) {
	data, err := json.Marshal(spec)
	if self == "" || err != nil {
		return binary, args
	}
	return self, append([]string{helperArg, string(data), binary}, args...)
}

// RunHelperIfAsked turns this process into the confined program, if this is what the binary was started for. Call it first
// thing in main. It does not return in that case.
func RunHelperIfAsked() {
	if len(os.Args) < 4 || os.Args[1] != helperArg {
		return
	}
	var spec Spec
	if err := json.Unmarshal([]byte(os.Args[2]), &spec); err != nil {
		fmt.Fprintln(os.Stderr, "confine: bad spec:", err)
		os.Exit(127)
	}
	binary := os.Args[3]
	if path, err := exec.LookPath(binary); err == nil {
		binary = path
	}
	// Landlock applies to the thread that asks, and exec keeps that thread's restrictions.
	runtime.LockOSThread()
	spec.Read = append(spec.Read, filepath.Dir(binary))
	if real, err := filepath.EvalSymlinks(binary); err == nil {
		spec.Read = append(spec.Read, filepath.Dir(real))
	}
	if err := apply(spec); err != nil && !errors.Is(err, errUnsupported) {
		fmt.Fprintln(os.Stderr, "confine:", err)
		os.Exit(127)
	}
	err := syscall.Exec(binary, append([]string{filepath.Base(binary)}, os.Args[4:]...), os.Environ())
	fmt.Fprintln(os.Stderr, "confine: could not start", binary+":", err)
	os.Exit(127)
}

// Protect makes this process undumpable, so another process of the same user cannot read its memory or its environment.
// What it starts is not affected: exec resets it. A child that has become a subreaper's orphan comes back here, which is
// what ReapOrphans is for.
func Protect() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("could not make this process undumpable: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("could not become a subreaper: %w", err)
	}
	return nil
}

// Status says how much Landlock can do here, for the log at startup.
func Status() string {
	abi, err := landlockABI()
	if err != nil {
		return "Landlock is not available here, so ffmpeg can reach every file this user can"
	}
	if abi < 4 {
		return fmt.Sprintf("Landlock version %d limits ffmpeg's files, but this kernel cannot limit its connections", abi)
	}
	return fmt.Sprintf("Landlock version %d limits ffmpeg's files and connections", abi)
}

// NoNewPrivileges reports whether the process already runs with no_new_privs. Landlock needs it to be set, and setting it
// turns off anything that relies on setuid, such as Chrome's setuid sandbox on a machine that uses it.
func NoNewPrivileges() bool {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "NoNewPrivs:"); ok {
			return strings.TrimSpace(v) == "1"
		}
	}
	return false
}

var errUnsupported = errors.New("Landlock is not available")

func landlockABI() (int, error) {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0, errUnsupported
	}
	return int(abi), nil
}

const (
	fsRead  = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	fsWrite = unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM
	// What can be granted on a file rather than a directory.
	fsOnFile = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
)

// The kernel's names for the kinds of rule.
const (
	rulePathBeneath = 1
	ruleNetPort     = 2
)

// netPortAttr is struct landlock_net_port_attr.
type netPortAttr struct {
	allowed uint64
	port    uint64
}

// apply limits this thread, and whatever it becomes, to what spec allows. It only ever narrows what the process could do.
func apply(spec Spec) error {
	abi, err := landlockABI()
	if err != nil {
		return err
	}
	var extra uint64
	if abi >= 2 {
		extra |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		extra |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		extra |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	write := uint64(fsRead|fsWrite) | extra
	// The struct grows with each version, and the kernel checks that any part it does not know about is zero.
	attr := unix.LandlockRulesetAttr{Handled_access_fs: write}
	size := unsafe.Sizeof(uint64(0))
	if abi >= 4 && !spec.AnyNetwork {
		attr.Handled_access_net = unix.LANDLOCK_ACCESS_NET_CONNECT_TCP | unix.LANDLOCK_ACCESS_NET_BIND_TCP
		size = 2 * unsafe.Sizeof(uint64(0))
	}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), size, 0)
	if errno != 0 {
		return fmt.Errorf("creating a ruleset: %w", errno)
	}
	defer unix.Close(int(fd))

	for _, path := range append(append([]string{}, systemRead...), spec.Read...) {
		if err := allowPath(int(fd), path, fsRead); err != nil {
			return err
		}
	}
	for _, path := range append(append([]string{}, systemWrite...), spec.Write...) {
		if err := allowPath(int(fd), path, write); err != nil {
			return err
		}
	}
	if attr.Handled_access_net != 0 {
		for _, port := range spec.Connect {
			rule := netPortAttr{allowed: unix.LANDLOCK_ACCESS_NET_CONNECT_TCP, port: uint64(port)}
			if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, ruleNetPort, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
				return fmt.Errorf("allowing port %d: %w", port, errno)
			}
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("setting no_new_privs: %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return fmt.Errorf("restricting this process: %w", errno)
	}
	return nil
}

func allowPath(ruleset int, path string, access uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // Not every system has all of these.
		}
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		access &= fsOnFile
	}
	rule := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)}
	_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), rulePathBeneath, uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("allowing %s: %w", path, errno)
	}
	return nil
}
