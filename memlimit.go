package main

import (
	"log"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// configureMemory sets GOMEMLIMIT for the detected memory ceiling unless the
// operator set one, so the GC defends a process-wide limit instead of letting
// the heap overshoot to ~2x the live set and OOM on a small (1 GB) box.
// Returns the limit in effect (0 = none).
func configureMemory() int64 {
	total := detectMemoryLimit()
	limit := debug.SetMemoryLimit(-1)
	switch {
	case limit != math.MaxInt64:
		log.Printf("Memory: respecting configured GOMEMLIMIT (%d MB)", limit>>20)
		return limit
	case total > 0:
		// Headroom for the OS, badger's mmaps and non-heap memory: 20% of
		// total, at least 256 MB.
		reserve := total / 5
		if reserve < 256<<20 {
			reserve = 256 << 20
		}
		if l := total - reserve; l > 0 {
			debug.SetMemoryLimit(l)
			log.Printf("Memory: detected %d MB, GOMEMLIMIT set to %d MB", total>>20, l>>20)
			return l
		}
	}
	log.Printf("Memory: could not detect a limit; leaving GOMEMLIMIT at default")
	return 0
}

// detectMemoryLimit returns the memory ceiling in bytes available to this
// process, preferring cgroup limits (containers) over host RAM. Returns 0 when
// no limit can be determined.
func detectMemoryLimit() int64 {
	if v := readCgroupV2(); v > 0 {
		return v
	}
	if v := readCgroupV1(); v > 0 {
		return v
	}
	return readMemTotal()
}

func readCgroupV2() int64 {
	b, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0 // unlimited
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

func readCgroupV1() int64 {
	b, err := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")
	if err != nil {
		return 0
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || v <= 0 {
		return 0
	}
	if v > (1 << 60) {
		return 0 // cgroup v1 sentinel for "unlimited"
	}
	return v
}

func readMemTotal() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				return kb * 1024 // /proc/meminfo reports kB
			}
		}
	}
	return 0
}
