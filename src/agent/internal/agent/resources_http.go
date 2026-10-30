package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"aico-agent/internal/mcu"
)

type resourceCPU struct {
	Total uint64 `json:"total"`
	Idle  uint64 `json:"idle"`
}

type resourceUsage struct {
	TotalBytes uint64 `json:"total_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
}

type resourceWiFi struct {
	Name    string   `json:"name"`
	Index   int      `json:"index"`
	IPs     []string `json:"ips"`
	RXBytes *uint64  `json:"rx_bytes"`
	TXBytes *uint64  `json:"tx_bytes"`
}

type resourceDisk struct {
	Path  string         `json:"path"`
	Usage *resourceUsage `json:"usage"`
}

type resourceSnapshot struct {
	SampledAtMS int64          `json:"sampled_at_ms"`
	BootID      string         `json:"boot_id"`
	WiFi        []resourceWiFi `json:"wifi"`
	Battery     *mcu.Battery   `json:"battery"`
	CPU         *resourceCPU   `json:"cpu"`
	NPUPercent  *float64       `json:"npu_percent"`
	Memory      *resourceUsage `json:"memory"`
	Disks       []resourceDisk `json:"disks"`
}

// This read-only snapshot is available wherever the Web UI is reachable.
// MCU control endpoints retain their USB/loopback restriction.
func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snapshot := resourceSnapshot{
		SampledAtMS: time.Now().UnixMilli(),
		BootID:      strings.TrimSpace(readResourceFile("/proc/sys/kernel/random/boot_id")),
		WiFi:        readResourceWiFi(),
		CPU:         parseResourceCPU(readResourceFile("/proc/stat")),
		Memory:      parseResourceMemory(readResourceFile("/proc/meminfo")),
		Disks:       readResourceDisks(),
	}
	for _, path := range []string{"/sys/kernel/debug/rknpu/load", "/proc/rknpu/load"} {
		if snapshot.NPUPercent = parseResourceNPU(readResourceFile(path)); snapshot.NPUPercent != nil {
			break
		}
	}
	// An absent or unresponsive MCU must not delay the one-second refresh.
	ctx, cancel := context.WithTimeout(r.Context(), 700*time.Millisecond)
	defer cancel()
	do := s.mcuRequest
	if do == nil {
		client := mcu.Client{Socket: strings.TrimSpace(os.Getenv("AICO_MCU_SERVICE_SOCKET"))}
		do = client.Do
	}
	if response, err := do(ctx, mcu.Request{Op: "get_battery"}); err == nil && response.Status == "OK" {
		snapshot.Battery = response.Battery
	}
	_ = json.NewEncoder(w).Encode(snapshot)
}

func readResourceFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func parseResourceCPU(data string) *resourceCPU {
	fields := strings.Fields(strings.SplitN(data, "\n", 2)[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return nil
	}
	sample := &resourceCPU{}
	// guest and guest_nice are already counted in user and nice.
	for i := 1; i < len(fields) && i <= 8; i++ {
		value, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return nil
		}
		sample.Total += value
		if i == 4 || i == 5 {
			sample.Idle += value
		}
	}
	return sample
}

func parseResourceMemory(data string) *resourceUsage {
	values := map[string]uint64{}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "kB" {
			continue
		}
		if value, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
			values[strings.TrimSuffix(fields[0], ":")] = value * 1024
		}
	}
	total, available := values["MemTotal"], values["MemAvailable"]
	if _, ok := values["MemAvailable"]; !ok || total == 0 || available > total {
		return nil
	}
	return &resourceUsage{TotalBytes: total, UsedBytes: total - available}
}

// Rockchip prints a blank number for zero load ("NPU load:  %").
var resourceNPUPercent = regexp.MustCompile(`([0-9]*)\s*%`)

func parseResourceNPU(data string) *float64 {
	if !strings.Contains(data, "NPU load:") {
		return nil
	}
	matches := resourceNPUPercent.FindAllStringSubmatch(data, -1)
	if len(matches) == 0 {
		return nil
	}
	var total float64
	for _, match := range matches {
		var value float64
		if match[1] != "" {
			var err error
			value, err = strconv.ParseFloat(match[1], 64)
			if err != nil || value > 100 {
				return nil
			}
		}
		total += value
	}
	average := total / float64(len(matches))
	return &average
}

func readResourceWiFi() []resourceWiFi {
	result := []resourceWiFi{}
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		base := filepath.Join("/sys/class/net", iface.Name)
		if _, err := os.Stat(filepath.Join(base, "wireless")); err != nil {
			if _, err := os.Stat(filepath.Join(base, "phy80211")); err != nil {
				continue
			}
		}
		sample := resourceWiFi{Name: iface.Name, Index: iface.Index, IPs: []string{}}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.IsGlobalUnicast() {
				sample.IPs = append(sample.IPs, ip.String())
			}
		}
		for name, target := range map[string]**uint64{"rx_bytes": &sample.RXBytes, "tx_bytes": &sample.TXBytes} {
			if value, err := strconv.ParseUint(strings.TrimSpace(readResourceFile(filepath.Join(base, "statistics", name))), 10, 64); err == nil {
				*target = &value
			}
		}
		result = append(result, sample)
	}
	return result
}

func resourceDiskPaths(mounts string) []string {
	paths := []string{"/"}
	seen := map[string]bool{"/": true}
	devices := map[string]bool{}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}
		// Bind mounts share capacity with their original block device.
		if devices[fields[0]] {
			continue
		}
		devices[fields[0]] = true
		path := unescape.Replace(fields[1])
		if !seen[path] {
			paths = append(paths, path)
			seen[path] = true
		}
	}
	return paths
}

func readResourceDisks() []resourceDisk {
	disks := []resourceDisk{}
	for _, path := range resourceDiskPaths(readResourceFile("/proc/mounts")) {
		disk := resourceDisk{Path: path}
		var stat syscall.Statfs_t
		if err := syscall.Statfs(path, &stat); err == nil && stat.Blocks > 0 && stat.Bfree <= stat.Blocks {
			disk.Usage = &resourceUsage{
				TotalBytes: stat.Blocks * uint64(stat.Bsize),
				UsedBytes:  (stat.Blocks - stat.Bfree) * uint64(stat.Bsize),
			}
		}
		disks = append(disks, disk)
	}
	return disks
}
