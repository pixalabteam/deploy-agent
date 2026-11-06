package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pixalabteam/deploy-agent/internal/dockerctl"
)

// Resources is a point-in-time snapshot for the admin UI. Host metrics come
// from /proc (Linux only); each section degrades to an Error string instead of
// failing the whole snapshot, so one unavailable source never blanks the page.
type Resources struct {
	Host       HostResources             `json:"host"`
	Containers []dockerctl.ContainerStat `json:"containers"`
	DockerDisk []dockerctl.DiskUsageRow  `json:"docker_disk"`
	Errors     []string                  `json:"errors,omitempty"`
}

type HostResources struct {
	CPUPercent     float64 `json:"cpu_percent"`
	Load1          float64 `json:"load_1"`
	Load5          float64 `json:"load_5"`
	Load15         float64 `json:"load_15"`
	MemTotalBytes  uint64  `json:"mem_total_bytes"`
	MemUsedBytes   uint64  `json:"mem_used_bytes"`
	DiskTotal      uint64  `json:"disk_total_bytes"`
	DiskUsed       uint64  `json:"disk_used_bytes"`
	NetRxBytes     uint64  `json:"net_rx_bytes"`
	NetTxBytes     uint64  `json:"net_tx_bytes"`
	DiskReadBytes  uint64  `json:"disk_read_bytes"`
	DiskWriteBytes uint64  `json:"disk_write_bytes"`
}

func (d *Deployer) CollectResources(ctx context.Context) Resources {
	var res Resources
	if err := fillHostResources(&res.Host); err != nil {
		res.Errors = append(res.Errors, "host: "+err.Error())
	}

	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stats, err := d.docker.Stats(sctx)
	if err != nil {
		res.Errors = append(res.Errors, "docker stats: "+err.Error())
	}
	res.Containers = stats
	disk, err := d.docker.DiskUsage(sctx)
	if err != nil {
		res.Errors = append(res.Errors, "docker system df: "+err.Error())
	}
	res.DockerDisk = disk
	return res
}

func fillHostResources(h *HostResources) error {
	busy1, total1, err := readCPUTimes()
	if err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	busy2, total2, err := readCPUTimes()
	if err != nil {
		return err
	}
	if total2 > total1 {
		h.CPUPercent = 100 * float64(busy2-busy1) / float64(total2-total1)
	}

	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		fmt.Sscanf(string(b), "%f %f %f", &h.Load1, &h.Load5, &h.Load15)
	}
	h.MemTotalBytes, h.MemUsedBytes = readMemory()
	h.NetRxBytes, h.NetTxBytes = readNetwork()
	h.DiskReadBytes, h.DiskWriteBytes = readDiskIO()

	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err == nil {
		h.DiskTotal = st.Blocks * uint64(st.Bsize)
		h.DiskUsed = (st.Blocks - st.Bavail) * uint64(st.Bsize)
	}
	return nil
}

func readCPUTimes() (busy, total uint64, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, fmt.Errorf("empty /proc/stat")
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, fmt.Errorf("unexpected /proc/stat format")
	}
	for i, v := range fields[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		total += n
		if i == 3 || i == 4 { // idle, iowait
			continue
		}
		busy += n
	}
	return busy, total, nil
}

func readMemory() (total, used uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	var available uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var key string
		var kb uint64
		fmt.Sscanf(sc.Text(), "%s %d", &key, &kb)
		switch key {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			available = kb * 1024
		}
	}
	if total > available {
		used = total - available
	}
	return total, used
}

// readNetwork sums rx/tx bytes over all non-loopback, non-virtual interfaces so
// container veth/bridge traffic is not counted twice.
func readNetwork() (rx, tx uint64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "br-") {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(fields[0], 10, 64)
		t, _ := strconv.ParseUint(fields[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx
}

// readDiskIO sums sectors (512 B each) over whole physical disks only; the
// partition rows would double count.
func readDiskIO() (read, written uint64) {
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || !isWholeDisk(fields[2]) {
			continue
		}
		r, _ := strconv.ParseUint(fields[5], 10, 64)
		w, _ := strconv.ParseUint(fields[9], 10, 64)
		read += r * 512
		written += w * 512
	}
	return read, written
}

func isWholeDisk(name string) bool {
	if _, err := os.Stat("/sys/block/" + name); err != nil {
		return false
	}
	return !strings.HasPrefix(name, "loop") && !strings.HasPrefix(name, "ram") && !strings.HasPrefix(name, "dm-")
}
