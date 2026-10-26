//go:build linux

package mcu

import (
	"errors"
	"io"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type serialPort struct {
	mu sync.Mutex
	fd int
}

func OpenSerial(path string) (io.ReadWriteCloser, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (io.ReadWriteCloser, error) { _ = unix.Close(fd); return nil, err }
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCEXCL, 0); err != nil {
		return fail(err)
	}
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return fail(err)
	}
	t.Iflag = 0
	t.Oflag = 0
	t.Lflag = 0
	t.Cflag = unix.B115200 | unix.CS8 | unix.CREAD | unix.CLOCAL
	t.Ispeed = unix.B115200
	t.Ospeed = unix.B115200
	t.Cc[unix.VMIN] = 0
	t.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		return fail(err)
	}
	return &serialPort{fd: fd}, nil
}
func (p *serialPort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fd < 0 {
		return nil
	}
	_ = unix.IoctlSetInt(p.fd, unix.TIOCNXCL, 0)
	err := unix.Close(p.fd)
	p.fd = -1
	return err
}
func (p *serialPort) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fd < 0 {
		return 0, io.ErrClosedPipe
	}
	poll := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}
	_, err := unix.Poll(poll, 10)
	if errors.Is(err, unix.EINTR) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if poll[0].Revents&unix.POLLIN == 0 {
		return 0, nil
	}
	n, err := unix.Read(p.fd, b)
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
		return 0, nil
	}
	if n < 0 {
		n = 0
	}
	return n, err
}
func (p *serialPort) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fd < 0 {
		return 0, io.ErrClosedPipe
	}
	written := 0
	deadline := time.Now().Add(20 * time.Millisecond)
	for written < len(b) {
		n, err := unix.Write(p.fd, b[written:])
		if n > 0 {
			written += n
		}
		if err != nil && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return written, err
		}
		if written == len(b) {
			return written, nil
		}
		if time.Now().After(deadline) {
			if written == 0 {
				return 0, ErrTransmitBusy
			}
			return written, errors.New("UART transmit deadline exceeded")
		}
		_, err = unix.Poll([]unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLOUT}}, 1)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return written, err
		}
	}
	return written, nil
}
func DrainSerial(port io.ReadWriteCloser) error {
	p, ok := port.(*serialPort)
	if !ok {
		return errors.New("not a UART")
	}
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		if p.fd < 0 {
			p.mu.Unlock()
			return io.ErrClosedPipe
		}
		count, err := unix.IoctlGetInt(p.fd, unix.TIOCOUTQ)
		p.mu.Unlock()
		if err != nil {
			return err
		}
		if count == 0 {
			time.Sleep(20 * time.Millisecond)
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return errors.New("UART drain deadline exceeded")
}
