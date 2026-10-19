package releaseowner

import (
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"time"
)

func waitSocket(file *os.File, event int16, end time.Time) error {
	for {
		remaining := time.Until(end)
		if remaining <= 0 {
			return errors.New("native frame deadline")
		}
		milliseconds := int(remaining.Milliseconds())
		if milliseconds < 1 {
			milliseconds = 1
		}
		fds := []unix.PollFd{{Fd: int32(file.Fd()), Events: event}}
		_, err := unix.Poll(fds, milliseconds)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if fds[0].Revents&(event|unix.POLLHUP|unix.POLLERR) != 0 {
			return nil
		}
	}
}
func readSocket(file *os.File, raw []byte, end time.Time) error {
	for len(raw) > 0 {
		if err := waitSocket(file, unix.POLLIN, end); err != nil {
			return err
		}
		n, err := unix.Read(int(file.Fd()), raw)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		raw = raw[n:]
	}
	return nil
}
func writeOutput(input *os.File, output io.Writer, raw []byte) (int, error) {
	file, ok := output.(*os.File)
	if !ok {
		return output.Write(raw)
	}
	end := time.Now().Add(5 * time.Second)
	total := 0
	for len(raw) > 0 {
		if err := waitSocket(file, unix.POLLOUT, end); err != nil {
			return total, err
		}
		n, err := unix.SendmsgN(int(file.Fd()), raw, nil, nil, unix.MSG_NOSIGNAL|unix.MSG_DONTWAIT)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
		total += n
		raw = raw[n:]
	}
	return total, nil
}
