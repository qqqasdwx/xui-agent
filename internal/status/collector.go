package status

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/config"
	"github.com/qqqasdwx/xui-agent/internal/xraybinary"
	"github.com/qqqasdwx/xui-agent/internal/xrayruntime"
	"github.com/qqqasdwx/xui-agent/internal/xrayupdate"
	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

type Collector struct {
	cfg            config.XrayConfig
	stateDirectory string
	startedAt      time.Time
	versionMu      sync.Mutex
	versionPath    string
	version        string
}

func NewCollector(cfg config.XrayConfig, stateDirectory string, startedAt time.Time) *Collector {
	return &Collector{cfg: cfg, stateDirectory: stateDirectory, startedAt: startedAt}
}

func (c *Collector) Heartbeat(ctx context.Context, agentVersion string, now time.Time) v1.Heartbeat {
	hostname, _ := os.Hostname()
	return v1.Heartbeat{
		ProtocolVersion: v1.Version,
		AgentVersion:    agentVersion,
		Hostname:        hostname,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		AgentStartedAt:  c.startedAt.Unix(),
		ClockUnixMilli:  now.UnixMilli(),
		System:          collectSystemInfo(c.configPath()),
		Xray:            c.collectXray(ctx),
		Capabilities:    []string{v1.CapabilityObserve},
	}
}

func collectSystemInfo(diskPath string) v1.SystemInfo {
	var out v1.SystemInfo
	if f, err := os.Open("/proc/uptime"); err == nil {
		var seconds float64
		if _, err := fmt.Fscan(f, &seconds); err == nil && seconds >= 0 {
			out.UptimeSeconds = uint64(seconds)
		}
		f.Close()
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		values := parseMemInfo(f)
		f.Close()
		out.MemoryTotal = values["MemTotal"] * 1024
		available := values["MemAvailable"] * 1024
		if out.MemoryTotal >= available {
			out.MemoryUsed = out.MemoryTotal - available
		}
	}
	if diskPath == "" {
		diskPath = "/"
	}
	if _, err := os.Stat(diskPath); err != nil {
		diskPath = "/"
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(diskPath, &stat); err == nil {
		out.DiskTotal = stat.Blocks * uint64(stat.Bsize)
		out.DiskAvailable = stat.Bavail * uint64(stat.Bsize)
	}
	return out
}

func parseMemInfo(r io.Reader) map[string]uint64 {
	values := make(map[string]uint64)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		values[strings.TrimSuffix(fields[0], ":")] = value
	}
	return values
}

func (c *Collector) collectXray(ctx context.Context) v1.XrayInfo {
	var out v1.XrayInfo
	binaryPath := c.binaryPath()
	if binaryPath != "" {
		if info, err := os.Stat(binaryPath); err == nil && !info.IsDir() {
			out.Present = true
			out.Version = c.xrayVersion(ctx, binaryPath)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			out.Error = "inspect xray binary: " + err.Error()
		}
	}
	configPath := c.configPath()
	if configPath != "" {
		digest, err := fileSHA256(configPath)
		if err == nil {
			out.ConfigDigest = digest
		} else if !errors.Is(err, os.ErrNotExist) {
			out.Error = appendError(out.Error, "inspect xray config: "+err.Error())
		}
	}
	if c.cfg.Managed() {
		binary, err := xrayupdate.LoadAppliedState(c.stateDirectory)
		if err == nil {
			out.ManagedVersion = binary.Version
		} else if !errors.Is(err, os.ErrNotExist) {
			out.Error = appendError(out.Error, "inspect applied Xray binary: "+err.Error())
		}
		binaryFailure, err := xrayupdate.LoadFailureState(c.stateDirectory)
		if err == nil {
			out.BinaryRecoveryStatus = binaryFailure.RecoveryStatus
			out.BinaryRecoveryErrorCode = binaryFailure.ErrorCode
		} else if !errors.Is(err, os.ErrNotExist) {
			out.Error = appendError(out.Error, "inspect failed Xray binary update: "+err.Error())
		}
		applied, err := xrayruntime.LoadAppliedState(c.stateDirectory)
		if err == nil {
			if out.ConfigDigest != applied.ConfigDigest {
				out.Error = appendError(out.Error, "managed Xray config differs from applied state")
			} else {
				out.ConfigVersion = applied.ConfigVersion
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			out.Error = appendError(out.Error, "inspect applied Xray config: "+err.Error())
		}
		failure, err := xrayruntime.LoadFailureState(c.stateDirectory)
		if err == nil {
			out.RecoveryStatus = failure.RecoveryStatus
			out.RecoveryErrorCode = failure.ErrorCode
		} else if !errors.Is(err, os.ErrNotExist) {
			out.Error = appendError(out.Error, "inspect failed Xray config state: "+err.Error())
		}
	}
	pidFile := c.cfg.PIDFile
	if c.cfg.Managed() {
		pidFile = xrayruntime.PIDPath(c.stateDirectory)
	}
	if pidFile != "" || binaryPath != "" {
		running, startedAt, err := processStatus(pidFile, binaryPath)
		out.Running = running
		out.StartedAt = startedAt
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			out.Error = appendError(out.Error, "inspect xray process: "+err.Error())
		}
	}
	return out
}

func (c *Collector) binaryPath() string {
	if c.cfg.Managed() {
		return xraybinary.ActivePath(c.stateDirectory, c.cfg.BinaryPath)
	}
	return c.cfg.BinaryPath
}

func (c *Collector) configPath() string {
	if c.cfg.Managed() {
		return xrayruntime.CurrentConfigPath(c.stateDirectory)
	}
	return c.cfg.ConfigPath
}

func (c *Collector) xrayVersion(ctx context.Context, selectedPath string) string {
	binaryPath, err := filepath.EvalSymlinks(selectedPath)
	if err != nil {
		binaryPath = filepath.Clean(selectedPath)
	}
	c.versionMu.Lock()
	defer c.versionMu.Unlock()
	if c.version != "" && c.versionPath == binaryPath {
		return c.version
	}
	versionCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(versionCtx, binaryPath, "version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	c.versionPath = binaryPath
	c.version = strings.TrimSpace(line)
	return c.version
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func processStatus(pidFile, binaryPath string) (bool, int64, error) {
	if pidFile == "" {
		return findProcessByExecutable(binaryPath)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return false, 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return false, 0, errors.New("invalid pid file")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, 0, nil
		}
		// EPERM means the process exists but is owned by another user. The
		// dedicated agent account normally observes an Xray process owned by
		// root or an xray service account.
		if !errors.Is(err, syscall.EPERM) {
			return false, 0, err
		}
	}
	if binaryPath != "" {
		matches, err := processExecutableMatches(pid, binaryPath)
		if err != nil {
			return false, 0, err
		}
		if !matches {
			return false, 0, errors.New("pid file points to a different executable")
		}
	}
	startedAt, err := processStartedAt(pid)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	return true, startedAt, err
}

func findProcessByExecutable(binaryPath string) (bool, int64, error) {
	if binaryPath == "" {
		return false, 0, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, 0, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		matches, err := processExecutableMatches(pid, binaryPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				continue
			}
			continue
		}
		if matches {
			startedAt, err := processStartedAt(pid)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return true, startedAt, err
		}
	}
	return false, 0, nil
}

func processStartedAt(pid int) (int64, error) {
	processRaw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	startTicks, err := parseProcessStartTicks(string(processRaw))
	if err != nil {
		return 0, err
	}
	bootRaw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	bootTime, err := parseBootTime(string(bootRaw))
	if err != nil {
		return 0, err
	}
	const linuxUserHZ uint64 = 100
	const maxInt64Seconds = uint64(1<<63 - 1)
	startSeconds := startTicks / linuxUserHZ
	if startSeconds > maxInt64Seconds-uint64(bootTime) {
		return 0, errors.New("process start time overflows Unix time")
	}
	return bootTime + int64(startSeconds), nil
}

func parseProcessStartTicks(raw string) (uint64, error) {
	commEnd := strings.LastIndexByte(raw, ')')
	if commEnd < 0 {
		return 0, errors.New("process stat has no command terminator")
	}
	fields := strings.Fields(raw[commEnd+1:])
	const startTimeIndexAfterCommand = 19
	if len(fields) <= startTimeIndexAfterCommand {
		return 0, errors.New("process stat is missing start time")
	}
	startTicks, err := strconv.ParseUint(fields[startTimeIndexAfterCommand], 10, 64)
	if err != nil {
		return 0, errors.New("process stat start time is invalid")
	}
	return startTicks, nil
}

func parseBootTime(raw string) (int64, error) {
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "btime" {
			continue
		}
		bootTime, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || bootTime <= 0 {
			return 0, errors.New("system boot time is invalid")
		}
		return bootTime, nil
	}
	return 0, errors.New("system boot time is missing")
}

func processExecutableMatches(pid int, binaryPath string) (bool, error) {
	want, err := filepath.EvalSymlinks(binaryPath)
	if err != nil {
		return false, err
	}
	processDirectory := filepath.Join("/proc", strconv.Itoa(pid))
	got, err := os.Readlink(filepath.Join(processDirectory, "exe"))
	if err != nil {
		if !errors.Is(err, os.ErrPermission) {
			return false, err
		}
		return processCommandMatches(filepath.Join(processDirectory, "cmdline"), want)
	}
	got = strings.TrimSuffix(got, " (deleted)")
	return filepath.Clean(got) == filepath.Clean(want), nil
}

func processCommandMatches(cmdlinePath, want string) (bool, error) {
	raw, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return false, err
	}
	arg0, _, _ := strings.Cut(string(raw), "\x00")
	if arg0 == "" || !filepath.IsAbs(arg0) {
		return false, nil
	}
	got, err := filepath.EvalSymlinks(arg0)
	if err != nil {
		return false, err
	}
	return filepath.Clean(got) == filepath.Clean(want), nil
}

func appendError(existing, next string) string {
	if existing == "" {
		return next
	}
	return existing + "; " + next
}
