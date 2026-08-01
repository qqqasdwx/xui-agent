package telemetry

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/qqqasdwx/xui-agent/internal/eventspool"
	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

const (
	accessCheckpointKey = "xray-access-v1"
	maxAccessLineBytes  = 64 << 10
)

var accessPattern = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2}\s+\d{2}:\d{2}:\d{2}(?:\.\d+)?)\s+from\s+(\S+)\s+accepted\s+(tcp|udp):(.+)\s+\[(.+?)\s+(?:->|>>)\s+(.+?)\]\s+email:\s+(\S+)\s*$`)

type AccessCollector struct {
	path     string
	interval time.Duration
	location *time.Location
	spool    *eventspool.Store
}

func NewAccessCollector(path string, interval time.Duration, timezone string, spool *eventspool.Store) (*AccessCollector, error) {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, err
	}
	return &AccessCollector{path: path, interval: interval, location: location, spool: spool}, nil
}

func (c *AccessCollector) Run(ctx context.Context) {
	c.collect(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collect(ctx)
		}
	}
}

func (c *AccessCollector) collect(ctx context.Context) {
	if err := c.collectOnce(ctx); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, eventspool.ErrFull) {
		_ = c.spool.RecordError(accessErrorCode(err), time.Now())
		slog.Warn("access collection failed", "reason", accessErrorCode(err))
	}
}

func (c *AccessCollector) collectOnce(ctx context.Context) error {
	currentInfo, err := os.Stat(c.path)
	if err != nil {
		return err
	}
	currentIdentity, err := fileIdentity(currentInfo)
	if err != nil {
		return err
	}
	checkpoint, err := c.spool.Checkpoint(accessCheckpointKey)
	if errors.Is(err, os.ErrNotExist) {
		checkpoint = eventspool.AccessCheckpoint{
			Device: currentIdentity.Device, Inode: currentIdentity.Inode,
			Offset: currentInfo.Size(), Initialized: true,
		}
		checkpoint, err = checkpointWithFileAnchor(c.path, checkpoint)
		if err != nil {
			return err
		}
		return c.spool.SetCheckpoint(accessCheckpointKey, checkpoint)
	}
	if err != nil {
		return err
	}

	if checkpoint.Device == currentIdentity.Device && checkpoint.Inode == currentIdentity.Inode {
		anchorOK, anchorErr := checkpointAnchorMatches(c.path, checkpoint)
		if anchorErr != nil {
			return anchorErr
		}
		if currentInfo.Size() < checkpoint.Offset || !anchorOK {
			checkpoint.Offset = 0
			checkpoint.AnchorOffset = 0
			checkpoint.AnchorSHA256 = ""
			if err := c.spool.SetCheckpoint(accessCheckpointKey, checkpoint); err != nil {
				return err
			}
		}
		_, err := c.drain(ctx, c.path, checkpoint)
		return err
	}

	rotated, findErr := findFileByIdentity(c.path, checkpoint.Device, checkpoint.Inode)
	if findErr != nil {
		return findErr
	}
	if rotated != "" {
		anchorOK, err := checkpointAnchorMatches(rotated, checkpoint)
		if err != nil {
			return err
		}
		if !anchorOK {
			rotated = ""
			_ = c.spool.RecordError("access_rotation_gap", time.Now())
		}
	}
	if rotated != "" {
		drained, err := c.drain(ctx, rotated, checkpoint)
		if err != nil || !drained {
			return err
		}
	} else {
		_ = c.spool.RecordError("access_rotation_gap", time.Now())
	}

	checkpoint = eventspool.AccessCheckpoint{
		Device: currentIdentity.Device, Inode: currentIdentity.Inode, Offset: 0, Initialized: true,
	}
	if err := c.spool.SetCheckpoint(accessCheckpointKey, checkpoint); err != nil {
		return err
	}
	_, err = c.drain(ctx, c.path, checkpoint)
	return err
}

func (c *AccessCollector) drain(ctx context.Context, path string, checkpoint eventspool.AccessCheckpoint) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	if _, err := file.Seek(checkpoint.Offset, io.SeekStart); err != nil {
		return false, err
	}
	reader := bufio.NewReaderSize(file, 16<<10)
	offset := checkpoint.Offset
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		line, readErr := readBoundedAccessLine(reader)
		if readErr != nil {
			return false, readErr
		}
		if !line.complete {
			return line.bytesRead == 0, nil
		}
		nextOffset := offset + line.bytesRead
		checkpoint.Offset = nextOffset
		checkpoint = checkpointWithLineAnchor(checkpoint, line.anchor)
		if line.tooLong {
			_ = c.spool.RecordError("access_line_too_long", time.Now())
			if err := c.spool.SetCheckpoint(accessCheckpointKey, checkpoint); err != nil {
				return false, err
			}
			offset = nextOffset
			continue
		}
		event, keep, parseErr := parseAccessLine(strings.TrimSuffix(string(line.data), "\n"), c.location)
		if parseErr != nil || !keep {
			if parseErr != nil {
				_ = c.spool.RecordError("access_parse_failed", time.Now())
			}
			if err := c.spool.SetCheckpoint(accessCheckpointKey, checkpoint); err != nil {
				return false, err
			}
		} else if _, err := c.spool.EnqueueWithCheckpoint(v1.EventKindAccess, event.observedAt, event.payload, accessCheckpointKey, checkpoint); err != nil {
			return false, err
		}
		offset = nextOffset
	}
}

type boundedAccessLine struct {
	data      []byte
	anchor    []byte
	bytesRead int64
	complete  bool
	tooLong   bool
}

func readBoundedAccessLine(reader *bufio.Reader) (boundedAccessLine, error) {
	var result boundedAccessLine
	for {
		fragment, err := reader.ReadSlice('\n')
		result.bytesRead += int64(len(fragment))
		result.anchor = appendAccessAnchor(result.anchor, fragment)
		if !result.tooLong {
			if result.bytesRead > maxAccessLineBytes {
				result.data = nil
				result.tooLong = true
			} else {
				result.data = append(result.data, fragment...)
			}
		}
		switch {
		case err == nil:
			result.complete = true
			return result, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return result, nil
		default:
			return boundedAccessLine{}, err
		}
	}
}

func appendAccessAnchor(anchor, fragment []byte) []byte {
	const anchorBytes = 64
	if len(fragment) >= anchorBytes {
		return append(anchor[:0], fragment[len(fragment)-anchorBytes:]...)
	}
	if overflow := len(anchor) + len(fragment) - anchorBytes; overflow > 0 {
		copy(anchor, anchor[overflow:])
		anchor = anchor[:len(anchor)-overflow]
	}
	return append(anchor, fragment...)
}

func checkpointWithLineAnchor(checkpoint eventspool.AccessCheckpoint, line []byte) eventspool.AccessCheckpoint {
	const anchorBytes = 64
	if len(line) > anchorBytes {
		line = line[len(line)-anchorBytes:]
	}
	checkpoint.AnchorOffset = checkpoint.Offset - int64(len(line))
	sum := sha256.Sum256(line)
	checkpoint.AnchorSHA256 = hex.EncodeToString(sum[:])
	return checkpoint
}

func checkpointWithFileAnchor(path string, checkpoint eventspool.AccessCheckpoint) (eventspool.AccessCheckpoint, error) {
	if checkpoint.Offset == 0 {
		return checkpoint, nil
	}
	const anchorBytes = int64(64)
	length := anchorBytes
	if checkpoint.Offset < length {
		length = checkpoint.Offset
	}
	file, err := os.Open(path)
	if err != nil {
		return checkpoint, err
	}
	defer file.Close()
	raw := make([]byte, length)
	if _, err := file.ReadAt(raw, checkpoint.Offset-length); err != nil {
		return checkpoint, err
	}
	checkpoint.AnchorOffset = checkpoint.Offset - length
	sum := sha256.Sum256(raw)
	checkpoint.AnchorSHA256 = hex.EncodeToString(sum[:])
	return checkpoint, nil
}

func checkpointAnchorMatches(path string, checkpoint eventspool.AccessCheckpoint) (bool, error) {
	if checkpoint.Offset == 0 || checkpoint.AnchorSHA256 == "" {
		return true, nil
	}
	length := checkpoint.Offset - checkpoint.AnchorOffset
	if length <= 0 || length > 64 {
		return false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	raw := make([]byte, length)
	if _, err := file.ReadAt(raw, checkpoint.AnchorOffset); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	sum := sha256.Sum256(raw)
	return subtleHexEqual(checkpoint.AnchorSHA256, hex.EncodeToString(sum[:])), nil
}

func subtleHexEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	var diff byte
	for i := range len(left) {
		diff |= left[i] ^ right[i]
	}
	return diff == 0
}

type parsedAccess struct {
	observedAt time.Time
	payload    v1.AccessEvent
}

func parseAccessLine(line string, location *time.Location) (parsedAccess, bool, error) {
	matches := accessPattern.FindStringSubmatch(strings.TrimSpace(line))
	if matches == nil {
		return parsedAccess{}, false, errors.New("unrecognized access line")
	}
	observedAt, err := time.ParseInLocation("2006/01/02 15:04:05.999999999", matches[1], location)
	if err != nil {
		return parsedAccess{}, false, err
	}
	sourceIP, sourcePort, err := splitSource(matches[2])
	if err != nil {
		return parsedAccess{}, false, err
	}
	targetHost, targetPort, err := splitTarget(matches[4])
	if err != nil {
		return parsedAccess{}, false, err
	}
	email, err := cleanToken(matches[7], 254, false)
	if err != nil {
		return parsedAccess{}, false, err
	}
	inbound, err := cleanToken(matches[5], 128, false)
	if err != nil {
		return parsedAccess{}, false, err
	}
	outbound, err := cleanToken(matches[6], 128, false)
	if err != nil {
		return parsedAccess{}, false, err
	}
	if inbound == "api" && outbound == "api" {
		return parsedAccess{}, false, nil
	}
	return parsedAccess{observedAt: observedAt, payload: v1.AccessEvent{
		Email: email, SourceIP: sourceIP, SourcePort: sourcePort, Network: matches[3],
		TargetHost: targetHost, TargetPort: targetPort, OutboundTag: outbound,
	}}, true, nil
}

func splitSource(value string) (string, uint16, error) {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", 0, errors.New("invalid access source")
	}
	address, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return "", 0, errors.New("invalid access source IP")
	}
	parsedPort, err := parsePort(port)
	if err != nil {
		return "", 0, err
	}
	return address.Unmap().String(), parsedPort, nil
}

func splitTarget(value string) (string, uint16, error) {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", 0, errors.New("invalid access target")
	}
	host, err = cleanHost(host)
	if err != nil {
		return "", 0, err
	}
	parsedPort, err := parsePort(port)
	return host, parsedPort, err
}

func splitXrayTarget(value string) (string, uint16, error) {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"tcp:", "udp:"} {
		if strings.HasPrefix(value, prefix) {
			value = strings.TrimPrefix(value, prefix)
			break
		}
	}
	if value == "" {
		return "", 0, nil
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", 0, errors.New("invalid Xray target")
	}
	host, err = cleanHost(host)
	if err != nil {
		return "", 0, err
	}
	parsedPort, err := parsePort(port)
	return host, parsedPort, err
}

func cleanHost(value string) (string, error) {
	value = strings.Trim(strings.TrimSpace(value), "[]")
	if address, err := netip.ParseAddr(value); err == nil {
		return address.Unmap().String(), nil
	}
	value = strings.TrimSuffix(strings.ToLower(value), ".")
	if value == "" || len(value) > 253 || !utf8.ValidString(value) {
		return "", errors.New("invalid target host")
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' || r == '\\' {
			return "", errors.New("invalid target host")
		}
	}
	return value, nil
}

func cleanToken(value string, max int, allowEmpty bool) (string, error) {
	value = strings.TrimSpace(value)
	if (!allowEmpty && value == "") || len(value) > max || !utf8.ValidString(value) {
		return "", errors.New("invalid event field")
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errors.New("invalid event field")
		}
	}
	return value, nil
}

func parsePort(value string) (uint16, error) {
	port, err := strconv.ParseUint(value, 10, 16)
	if err != nil || port == 0 {
		return 0, errors.New("invalid port")
	}
	return uint16(port), nil
}

type fileID struct {
	Device uint64
	Inode  uint64
}

func fileIdentity(info os.FileInfo) (fileID, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, errors.New("access file identity is unavailable")
	}
	return fileID{Device: uint64(stat.Dev), Inode: stat.Ino}, nil
}

func findFileByIdentity(path string, device, inode uint64) (string, error) {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		candidate := filepath.Join(filepath.Dir(path), entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		got, err := fileIdentity(info)
		if err == nil && got.Device == device && got.Inode == inode {
			return candidate, nil
		}
	}
	return "", nil
}

func accessErrorCode(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "access_file_missing"
	case errors.Is(err, eventspool.ErrFull):
		return "event_spool_full"
	default:
		return "access_read_failed"
	}
}

func formatAccessForRisk(event v1.AccessEvent, observedAt time.Time) string {
	return fmt.Sprintf("%s from %s accepted %s:%s:%d [agent -> %s] email: %s",
		observedAt.Format("2006/01/02 15:04:05.999999"), net.JoinHostPort(event.SourceIP, strconv.Itoa(int(event.SourcePort))),
		event.Network, event.TargetHost, event.TargetPort, event.OutboundTag, event.Email)
}
