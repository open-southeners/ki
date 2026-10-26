package monitor

import "time"

// HostSample is one host measurement. Rate fields are nil (JSON null) when
// unknown: on the first sample or when a counter went backwards.
type HostSample struct {
	Time    time.Time   `json:"time"`
	CPU     HostCPU     `json:"cpu"`
	Load    HostLoad    `json:"load"`
	Memory  HostMemory  `json:"memory"`
	Swap    HostSwap    `json:"swap"`
	Disks   []HostDisk  `json:"disks"`
	DiskIO  HostDiskIO  `json:"disk_io"`
	Network HostNetwork `json:"network"`
	UptimeS uint64      `json:"uptime_s"`
}

// HostCPU is CPU utilisation over all cores.
type HostCPU struct {
	Percent *float64  `json:"percent"`
	Cores   int       `json:"cores"`
	PerCore []float64 `json:"per_core,omitempty"`
}

// HostLoad holds the 1, 5 and 15 minute load averages.
type HostLoad struct {
	One     float64 `json:"1"`
	Five    float64 `json:"5"`
	Fifteen float64 `json:"15"`
}

// HostMemory is physical memory usage in bytes.
type HostMemory struct {
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Available uint64  `json:"available"`
	Percent   float64 `json:"percent"`
}

// HostSwap is swap usage in bytes.
type HostSwap struct {
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Percent float64 `json:"percent"`
}

// HostDisk is the usage of one filesystem.
type HostDisk struct {
	Path          string  `json:"path"`
	Device        string  `json:"device"`
	Fstype        string  `json:"fstype"`
	Total         uint64  `json:"total"`
	Used          uint64  `json:"used"`
	Free          uint64  `json:"free"`
	Percent       float64 `json:"percent"`
	InodesPercent float64 `json:"inodes_percent"`
}

// HostDiskIO is the throughput summed over whole block devices.
type HostDiskIO struct {
	ReadBytesPerS  *float64 `json:"read_bytes_per_s"`
	WriteBytesPerS *float64 `json:"write_bytes_per_s"`
	ReadOpsPerS    *float64 `json:"read_ops_per_s"`
	WriteOpsPerS   *float64 `json:"write_ops_per_s"`
}

// HostNetwork is the traffic summed over the counted interfaces plus the
// per-interface breakdown.
type HostNetwork struct {
	RxBytesPerS *float64        `json:"rx_bytes_per_s"`
	TxBytesPerS *float64        `json:"tx_bytes_per_s"`
	Interfaces  []HostInterface `json:"interfaces"`
}

// HostInterface is the traffic of one network interface.
type HostInterface struct {
	Name        string   `json:"name"`
	RxBytesPerS *float64 `json:"rx_bytes_per_s"`
	TxBytesPerS *float64 `json:"tx_bytes_per_s"`
}

// WithoutPerCore returns a copy of s without the per-core CPU breakdown.
func (s HostSample) WithoutPerCore() HostSample {
	s.CPU.PerCore = nil
	return s
}
