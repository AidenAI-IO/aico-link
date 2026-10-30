package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/mcu"
)

func TestResourceCPUAndMemory(t *testing.T) {
	cpu := parseResourceCPU("cpu 100 20 30 400 10 5 6 7 80 10\ncpu0 1 2 3 4\n")
	if cpu == nil || cpu.Total != 578 || cpu.Idle != 410 {
		t.Fatalf("CPU must count iowait as idle and exclude duplicate guest ticks: %+v", cpu)
	}
	for _, input := range []string{"", "cpu0 1 2 3 4", "cpu 1 2 3 bad"} {
		if parseResourceCPU(input) != nil {
			t.Fatalf("accepted invalid CPU sample %q", input)
		}
	}
	memory := parseResourceMemory("MemTotal: 1024 kB\nMemFree: 100 kB\nMemAvailable: 256 kB\n")
	if memory == nil || memory.TotalBytes != 1048576 || memory.UsedBytes != 786432 {
		t.Fatalf("memory must use MemAvailable rather than MemFree: %+v", memory)
	}
	for _, input := range []string{"", "MemTotal: 1024 kB", "MemTotal: 1 kB\nMemAvailable: 2 kB", "MemTotal: 100 kB\nMemAvailable: bad kB"} {
		if parseResourceMemory(input) != nil {
			t.Fatalf("accepted invalid memory sample %q", input)
		}
	}
}

func TestResourceNPU(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  float64
	}{
		{"NPU load:  9%\n", 9},
		{"NPU load:   %\n", 0},
		{"NPU load: Core0: 20%, Core1: 60%, Core2:  %", 80.0 / 3},
	} {
		if got := parseResourceNPU(tc.input); got == nil || *got != tc.want {
			t.Fatalf("parse %q = %v, want %v", tc.input, got, tc.want)
		}
	}
	for _, input := range []string{"", "NPU load: unavailable", "NPU load: 101%", "other: 12%"} {
		if parseResourceNPU(input) != nil {
			t.Fatalf("accepted invalid NPU sample %q", input)
		}
	}
}

func TestResourceDiskPaths(t *testing.T) {
	mounts := "/dev/root / ext4 rw 0 0\nproc /proc proc rw 0 0\n/dev/mmcblk0p9 /userdata ext4 rw 0 0\n/dev/mmcblk1p1 /mnt/sd\\040card vfat rw 0 0\n/dev/mmcblk0p9 /userdata ext4 rw 0 0\n/dev/mmcblk0p9 /home ext4 rw 0 0\n/dev/mmcblk0p9 /root ext4 rw 0 0\nhost:/share /network nfs rw 0 0"
	if got := resourceDiskPaths(mounts); !reflect.DeepEqual(got, []string{"/", "/userdata", "/mnt/sd card"}) {
		t.Fatalf("disk paths = %v", got)
	}
}

func TestResourcesHTTP(t *testing.T) {
	percent, voltage := byte(75), uint16(3980)
	calls := 0
	s := &Server{
		bridge: NewPhoneBridge(newTestLogger()),
		mcuRequest: func(ctx context.Context, request mcu.Request) (mcu.Response, error) {
			calls++
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 700*time.Millisecond || request.Op != "get_battery" {
				t.Fatalf("unbounded or non-battery MCU request: %+v", request)
			}
			return mcu.Response{Status: "OK", Battery: &mcu.Battery{Percent: &percent, Millivolts: &voltage}}, nil
		},
	}
	handler := s.Handler()
	r := httptest.NewRequest(http.MethodGet, "/api/resources", nil)
	r.RemoteAddr = "192.168.1.50:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var snapshot resourceSnapshot
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &snapshot) != nil {
		t.Fatalf("snapshot response: %d %s", w.Code, w.Body.String())
	}
	if snapshot.Battery == nil || *snapshot.Battery.Percent != percent || *snapshot.Battery.Millivolts != voltage || snapshot.SampledAtMS <= 0 {
		t.Fatalf("snapshot missing readings: %+v", snapshot)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("snapshot headers: %v", w.Header())
	}
	before := calls
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/resources", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "GET" || calls != before {
		t.Fatal("resource endpoint must reject mutations before touching MCU")
	}
	s.mcuRequest = func(context.Context, mcu.Request) (mcu.Response, error) {
		return mcu.Response{}, errors.New("MCU disconnected")
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"battery":null`) || !strings.Contains(w.Body.String(), `"disks":`) {
		t.Fatalf("missing MCU must preserve other metrics: %s", w.Body.String())
	}
}
