package agent

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type GPIOEdge string

const (
	GPIOEdgeFalling GPIOEdge = "falling"
	GPIOEdgeRising  GPIOEdge = "rising"
	GPIOEdgeBoth    GPIOEdge = "both"
)

// GPIOWatcher watches a GPIO pin for edge events
type GPIOWatcher struct {
	mu        sync.Mutex
	started   bool
	stopped   bool
	done      chan struct{}
	pin       int
	valuePath string
	fd        int
	callback  func()
	stopChan  chan struct{}
}

// NewGPIOWatcher creates a new GPIO watcher
func NewGPIOWatcher(pin int, callback func()) (*GPIOWatcher, error) {
	return NewGPIOWatcherWithEdge(pin, GPIOEdgeFalling, callback)
}

// NewGPIOWatcherWithEdge creates a GPIO watcher for a selected sysfs edge.
func NewGPIOWatcherWithEdge(pin int, edge GPIOEdge, callback func()) (*GPIOWatcher, error) {
	if pin < 0 {
		return nil, fmt.Errorf("GPIO pin must be >= 0, got %d", pin)
	}
	switch edge {
	case GPIOEdgeFalling, GPIOEdgeRising, GPIOEdgeBoth:
	default:
		return nil, fmt.Errorf("unsupported GPIO edge %q", edge)
	}
	w := &GPIOWatcher{
		pin:       pin,
		valuePath: fmt.Sprintf("/sys/class/gpio/gpio%d/value", pin),
		callback:  callback,
		stopChan:  make(chan struct{}),
	}

	// Export GPIO if not already exported
	if err := w.exportGPIO(); err != nil {
		return nil, fmt.Errorf("export GPIO: %w", err)
	}

	// Set direction to input
	if err := w.setDirection("in"); err != nil {
		return nil, fmt.Errorf("set direction: %w", err)
	}

	if err := w.setEdge(string(edge)); err != nil {
		return nil, fmt.Errorf("set edge: %w", err)
	}

	return w, nil
}

// exportGPIO exports the GPIO pin
func (w *GPIOWatcher) exportGPIO() error {
	// Check if already exported
	if _, err := os.Stat(fmt.Sprintf("/sys/class/gpio/gpio%d", w.pin)); err == nil {
		return nil // Already exported
	}

	f, err := os.OpenFile("/sys/class/gpio/export", os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open export: %w", err)
	}
	defer f.Close()

	pinStr := strconv.Itoa(w.pin)
	if _, err := f.WriteString(pinStr); err != nil {
		return fmt.Errorf("write pin: %w", err)
	}

	// Wait for GPIO to be ready
	time.Sleep(100 * time.Millisecond)
	return nil
}

// setDirection sets the GPIO direction
func (w *GPIOWatcher) setDirection(direction string) error {
	path := fmt.Sprintf("/sys/class/gpio/gpio%d/direction", w.pin)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open direction: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(direction); err != nil {
		return fmt.Errorf("write direction: %w", err)
	}

	return nil
}

// setEdge sets the GPIO edge trigger
func (w *GPIOWatcher) setEdge(edge string) error {
	path := fmt.Sprintf("/sys/class/gpio/gpio%d/edge", w.pin)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open edge: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(edge); err != nil {
		return fmt.Errorf("write edge: %w", err)
	}

	return nil
}

// Start starts watching the GPIO pin
func (w *GPIOWatcher) Start() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return fmt.Errorf("GPIO watcher has stopped")
	}
	if w.started {
		return nil
	}
	// Open GPIO value file
	fd, err := unix.Open(w.valuePath, unix.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open value: %w", err)
	}
	w.fd = fd
	w.started = true
	w.done = make(chan struct{})

	// Start watching in a goroutine
	go func() {
		defer close(w.done)
		w.watch(fd)
	}()

	return nil
}

// watch watches for GPIO events
func (w *GPIOWatcher) watch(fd int) {
	buf := make([]byte, 64)

	for {
		select {
		case <-w.stopChan:
			return
		default:
		}

		// Seek to beginning
		unix.Seek(fd, 0, 0)

		// Read current value
		unix.Read(fd, buf)

		// Poll for events
		fds := []unix.PollFd{
			{
				Fd:     int32(fd),
				Events: unix.POLLPRI | unix.POLLERR,
			},
		}

		// Wait for event (with timeout to check stopChan)
		n, err := unix.Poll(fds, 1000) // 1 second timeout
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}

		if n > 0 && (fds[0].Revents&unix.POLLPRI) != 0 {
			select {
			case <-w.stopChan:
				return
			default:
			}
			// GPIO event detected
			if w.callback != nil {
				w.callback()
			}

			// Debounce: wait 150ms before next event
			time.Sleep(150 * time.Millisecond)
		}
	}
}

// Stop stops watching the GPIO pin
func (w *GPIOWatcher) Stop() {
	w.mu.Lock()
	wasStopped := w.stopped
	w.stopped = true
	if !wasStopped {
		close(w.stopChan)
	}
	started, done, fd := w.started, w.done, w.fd
	w.mu.Unlock()
	if started {
		// Join before closing: a new watcher may otherwise reuse this FD
		// while the retired polling goroutine still reads from it.
		<-done
		if !wasStopped {
			unix.Close(fd)
		}
	}
}
