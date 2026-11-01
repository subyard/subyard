package incusclient

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The real opener is a detached UI attempt with no authority descriptors. Its
// lifetime is deliberately independent of the foreground SDK/bridge custody.
func launchOwnerSourceBrowser(environment []string, directory, uri string) error {
	if len(uri) == 0 || len(uri) > 16<<10 {
		return errors.New("browser URI bound")
	}
	u, err := url.Parse(uri)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("browser URI refusal")
	}
	values := map[string]string{}
	for _, s := range environment {
		k, v, ok := strings.Cut(s, "=")
		if ok {
			values[k] = v
		}
	}
	browser := values["BROWSER"]
	if browser == "none" {
		return nil
	}
	if browser == "" {
		browser = "xdg-open"
	}
	path, err := ownerSourceExecutable(browser, values["PATH"], directory)
	if err != nil {
		return err
	}
	command := exec.Command(path, uri)
	command.Env = ownerSourceEnvironment(environment)
	command.Dir = directory
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
func ownerSourceExecutable(name, path, directory string) (string, error) {
	if strings.Contains(name, "/") {
		if !filepath.IsAbs(name) {
			name = filepath.Join(directory, name)
		}
		info, err := os.Stat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			return "", errors.New("original executable unavailable")
		}
		return name, nil
	}
	for _, part := range filepath.SplitList(path) {
		if !filepath.IsAbs(part) {
			part = filepath.Join(directory, part)
		}
		candidate := filepath.Join(part, name)
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("original executable unavailable")
}

// Only OIDC installs this SDK-compatible one-executable BROWSER bridge. Socket
// peer credentials identify the exact direct SDK child for Wait4 (SDK uses
// Start without Wait). No URI/code is written to a durable fact.
func (leaf *ownerSourceLeaf) oidcBrowser(ctx context.Context) (func() error, error) {
	root, err := os.MkdirTemp("/tmp", "subyard-owner-browser-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		os.Remove(root)
		return nil, err
	}
	path := filepath.Join(root, "socket")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		os.Remove(root)
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		os.Remove(root)
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		var result error
		defer func() { done <- result }()
		for attempts := 0; attempts < 16; attempts++ {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			err = leaf.browserAttempt(ctx, conn)
			conn.Close()
			if err != nil {
				result = err
				return
			}
		}
		result = errors.New("browser attempt bound")
	}()
	oldBrowser, hadBrowser := os.LookupEnv("BROWSER")
	oldMarker, hadMarker := os.LookupEnv(ownerBrowserMarker)
	image, err := os.Executable()
	if err != nil {
		listener.Close()
		<-done
		os.Remove(path)
		os.Remove(root)
		return nil, err
	}
	// /proc/self/exe would be resolved in the SDK child before exec; use our
	// retained running image via this leaf's process instead of any live path.
	_ = image
	executable := filepath.Join("/proc", fmt.Sprint(os.Getpid()), "exe")
	if err := os.Setenv("BROWSER", executable); err != nil {
		return nil, err
	}
	_ = os.Setenv(ownerBrowserMarker, path)
	return func() error {
		if hadBrowser {
			_ = os.Setenv("BROWSER", oldBrowser)
		} else {
			_ = os.Unsetenv("BROWSER")
		}
		if hadMarker {
			_ = os.Setenv(ownerBrowserMarker, oldMarker)
		} else {
			_ = os.Unsetenv(ownerBrowserMarker)
		}
		listener.Close()
		err := <-done
		return errors.Join(err, os.Remove(path), os.Remove(root))
	}, nil
}
func (leaf *ownerSourceLeaf) browserAttempt(ctx context.Context, conn *net.UnixConn) (resultErr error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var peerErr error
	_ = raw.Control(func(fd uintptr) { cred, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	if peerErr != nil || cred == nil || cred.Uid != uint32(os.Getuid()) {
		return errors.New("browser peer refusal")
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cred.Pid))
	if err != nil {
		return err
	}
	end := strings.LastIndexByte(string(stat), ')')
	fields := strings.Fields(string(stat[end+1:]))
	if end < 0 || len(fields) < 2 || fields[1] != fmt.Sprint(os.Getpid()) {
		return errors.New("browser is not direct SDK child")
	}
	// Every authenticated direct SDK child is reaped, including truncated URI
	// or lost acknowledgement. Foreign peers are never waited on.
	defer func() {
		_ = conn.Close()
		resultErr = errors.Join(resultErr, waitOwnerSourceBrowserChild(ctx, int(cred.Pid)))
	}()

	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	var h [4]byte
	if _, err := io.ReadFull(conn, h[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n == 0 || n > 16<<10 {
		return errors.New("browser URI bound")
	}
	uri := make([]byte, n)
	if _, err := io.ReadFull(conn, uri); err != nil {
		return err
	}
	_, launchErr := leaf.wire.call(ownerSourceMessage{Kind: "browser", URI: string(uri)})
	b := byte(0)
	if launchErr != nil {
		b = 1
	}
	_, err = conn.Write([]byte{b})
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}
func waitOwnerSourceBrowserChild(ctx context.Context, pid int) error {
	// Reap only this authenticated direct child, never wait-any or a browser.
	for {
		var status syscall.WaitStatus
		found, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if err != nil {
			return err
		}
		if found == pid {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// RunOwnerBrowserBridge accepts precisely one URI and has no SDK/mutation API.
func RunOwnerBrowserBridge(path, uri string) error {
	if !filepath.IsAbs(path) || filepath.Base(path) != "socket" || !strings.HasPrefix(filepath.Base(filepath.Dir(path)), "subyard-owner-browser-") || len(uri) == 0 || len(uri) > 16<<10 {
		return errors.New("browser bridge invocation")
	}
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return errors.New("browser private endpoint")
	}
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(uri)))
	if _, err := conn.Write(h[:]); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, uri); err != nil {
		return err
	}
	var ack [1]byte
	if _, err := io.ReadFull(conn, ack[:]); err != nil {
		return err
	}
	if ack[0] != 0 {
		return errors.New("browser opener attempt failed")
	}
	return nil
}
