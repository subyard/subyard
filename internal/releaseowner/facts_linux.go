package releaseowner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"strings"
)

type Intent struct {
	Version  int     `json:"version"`
	Binding  Binding `json:"binding"`
	Owner    string  `json:"owner"`
	Resource string  `json:"resource"`
	Expected string  `json:"expected"`
	Desired  string  `json:"desired"`
	Source   string  `json:"source"`
	Recipe   string  `json:"recipe"`
}

type nativeReservation struct {
	Version int     `json:"version"`
	Binding Binding `json:"binding"`
	Files   int     `json:"files"`
	Bytes   int64   `json:"bytes"`
}

func reservationAt(directory int, binding Binding) (nativeReservation, error) {
	raw, err := readFactAt(directory, factName(binding.Ordinal, "reservation"))
	if err != nil {
		return nativeReservation{}, err
	}
	var value nativeReservation
	if strict(raw, &value) != nil || value.Version != 1 || value.Binding != binding || value.Files != 2570 || value.Bytes != 16<<20 {
		return value, errors.New("native reservation binding")
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(append(canonical, '\n'), raw) {
		return value, errors.New("noncanonical native reservation")
	}
	return value, nil
}

func reservationCapacity(directory int, binding Binding, reservation nativeReservation, pending, final string, size int64) error {
	fd, err := unix.Openat(directory, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), "native reservation inventory")
	defer file.Close()
	names, err := file.Readdirnames(8193)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(names) > 8192 {
		return errors.New("native reservation inventory bound")
	}
	prefix := fmt.Sprintf("%03d.", binding.Ordinal)
	files, bytes := 0, int64(0)
	pendingFound, finalFound := false, false
	for _, name := range names {
		if !strings.HasPrefix(strings.TrimPrefix(name, "."), prefix) {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(directory, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		id := Identity{uint64(stat.Dev), stat.Ino, stat.Mode, stat.Uid, uint64(stat.Nlink)}
		if !regular(id) || stat.Size < 0 || stat.Size > 1<<20 {
			return errors.New("unsafe native reservation leaf")
		}
		files++
		bytes += stat.Size
		pendingFound = pendingFound || name == pending
		finalFound = finalFound || name == final
	}
	if !pendingFound && !finalFound {
		files++
		bytes += size
	}
	if files > reservation.Files || bytes > reservation.Bytes {
		return errors.New("native reservation exhausted")
	}
	return nil
}

func privateDirectory(fd int) error {
	var s unix.Stat_t
	if e := unix.Fstat(fd, &s); e != nil {
		return e
	}
	if s.Mode&unix.S_IFMT != unix.S_IFDIR || s.Uid != uint32(os.Getuid()) || s.Mode&0o777 != 0o700 {
		return errors.New("unsafe native facts directory")
	}
	return nil
}
func (s *session) factsDirectory(binding Binding) (int, error) {
	current, e := unix.Openat(int(s.root.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return -1, e
	}
	for _, name := range []string{"migrations", "native-custody", "v1", binding.Transaction} {
		next, e := unix.Openat(current, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(current)
		if e != nil {
			return -1, e
		}
		if e = privateDirectory(next); e != nil {
			unix.Close(next)
			return -1, e
		}
		current = next
	}
	return current, nil
}
func readFactAt(parent int, name string) ([]byte, error) {
	fd, e := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	file := os.NewFile(uintptr(fd), "native fact")
	defer file.Close()
	id, e := IdentityOf(file)
	if e != nil || !regular(id) {
		return nil, errors.New("unsafe native fact")
	}
	var stat unix.Stat_t
	if e = unix.Fstat(fd, &stat); e != nil || stat.Size <= 0 || stat.Size > 1<<20 {
		return nil, errors.New("native fact bound")
	}
	raw, e := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if e != nil || len(raw) > 1<<20 {
		return nil, errors.New("native fact read")
	}
	return raw, nil
}
func factName(ordinal uint64, kind string) string { return fmt.Sprintf("%03d.%s.json", ordinal, kind) }
func (s *session) checkIntent(binding Binding, owner, resource, recipe string) error {
	directory, e := s.factsDirectory(binding)
	if e != nil {
		return e
	}
	defer unix.Close(directory)
	raw, e := readFactAt(directory, factName(binding.Ordinal, "intent"))
	if e != nil {
		return e
	}
	var intent Intent
	if strict(raw, &intent) != nil || intent.Version != 1 || intent.Binding != binding || intent.Owner != owner || intent.Resource != resource || !fingerprint(intent.Expected) || !fingerprint(intent.Desired) || !fingerprint(intent.Source) || intent.Recipe != recipe {
		return errors.New("native intent binding")
	}
	canonical, e := json.Marshal(intent)
	if e != nil || !bytes.Equal(append(canonical, '\n'), raw) {
		return errors.New("noncanonical native intent")
	}
	return nil
}
func (s *session) readTerminal(binding Binding) (*Terminal, error) {
	directory, e := s.factsDirectory(binding)
	if e != nil {
		return nil, e
	}
	defer unix.Close(directory)
	raw, e := readFactAt(directory, factName(binding.Ordinal, "terminal"))
	if e != nil {
		return nil, e
	}
	var terminal Terminal
	if strict(raw, &terminal) != nil || terminal.Version != 1 || terminal.Binding != binding || (terminal.State != "complete" && terminal.State != "failed") {
		return nil, errors.New("native terminal binding")
	}
	canonical, e := json.Marshal(terminal)
	if e != nil || !bytes.Equal(append(canonical, '\n'), raw) {
		return nil, errors.New("noncanonical native terminal")
	}
	return &terminal, nil
}
func (s *session) writeTerminal(terminal Terminal) error {
	return s.writeFact(terminal.Binding, "terminal", terminal)
}
func (s *session) writeFact(binding Binding, kind string, value any) error {
	directory, e := s.factsDirectory(binding)
	if e != nil {
		return e
	}
	defer unix.Close(directory)
	reservation, e := reservationAt(directory, binding)
	if e != nil {
		return e
	}
	raw, e := json.Marshal(value)
	limit := 4 << 10
	if kind == "terminal" {
		limit = 1 << 20
	}
	if e != nil || len(raw)+1 > limit {
		return errors.New("native fact encoding")
	}
	raw = append(raw, '\n')
	name := factName(binding.Ordinal, kind)
	pending := "." + name + ".pending"
	if e := reservationCapacity(directory, binding, reservation, pending, name, int64(len(raw))); e != nil {
		return e
	}
	if current, e := readFactAt(directory, name); e == nil {
		if !bytes.Equal(current, raw) {
			return errors.New("native fact collision")
		}
		return unix.Fsync(directory)
	} else if !errors.Is(e, unix.ENOENT) {
		return e
	}
	fd, e := unix.Openat(directory, pending, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if errors.Is(e, unix.EEXIST) {
		// A crash after fsync but before publication may leave the exact complete
		// staging fact. Only that byte-identical fact is eligible for retry;
		// partial, foreign or ambiguous staging remains UNKNOWN.
		current, readErr := readFactAt(directory, pending)
		if readErr != nil || !bytes.Equal(current, raw) {
			return errors.New("native pending fact collision")
		}
		fd, e = unix.Openat(directory, pending, unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	}
	if e != nil {
		return e
	}
	file := os.NewFile(uintptr(fd), "native pending fact")
	defer file.Close()
	id, e := IdentityOf(file)
	if e != nil || !regular(id) {
		return errors.New("unsafe native pending fact")
	}
	if e = unix.Fchmod(fd, 0o600); e != nil {
		return e
	}
	if n, writeErr := file.Write(raw); writeErr != nil {
		return writeErr
	} else if n != len(raw) {
		return io.ErrShortWrite
	}
	if e = file.Truncate(int64(len(raw))); e != nil {
		return e
	}
	if e = file.Sync(); e != nil {
		return e
	}
	if e = unix.Renameat2(directory, pending, directory, name, unix.RENAME_NOREPLACE); e != nil {
		if !errors.Is(e, unix.EEXIST) {
			return e
		}
		current, readErr := readFactAt(directory, name)
		if readErr != nil || !bytes.Equal(current, raw) {
			return errors.New("native fact publication collision")
		}
		if e = unix.Unlinkat(directory, pending, 0); e != nil {
			return e
		}
	}
	return unix.Fsync(directory)
}
