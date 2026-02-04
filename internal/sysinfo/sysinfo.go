// Package sysinfo collects static system information for the RMM agent.
//
// This package gathers OS, hardware, and platform details that don't change
// frequently (unlike stats which are collected every minute). This information
// is sent during registration and periodically updated to track system changes
// like OS upgrades or hardware modifications.
//
// Information collected:
//   - OS: linux, darwin, windows, etc.
//   - Platform: ubuntu, centos, debian, etc.
//   - Platform family: debian, rhel, etc.
//   - Platform version: 22.04, 9, etc.
//   - Kernel version: 5.15.0-generic, etc.
//   - Architecture: amd64, arm64, etc.
//   - Virtualization: kvm, docker, vmware, etc.
//   - CPU model and count
//   - Total memory
//   - Host ID (machine UUID)
package sysinfo

import (
	"context"
	"runtime"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

// SystemInfo contains static system information about the host.
// This is collected once at startup and periodically refreshed.
type SystemInfo struct {
	// OS is the operating system name (linux, darwin, windows)
	OS string `json:"os"`

	// Platform is the distribution name (ubuntu, centos, debian, alpine)
	Platform string `json:"platform"`

	// PlatformFamily is the distribution family (debian, rhel, arch)
	PlatformFamily string `json:"platformFamily"`

	// PlatformVersion is the distribution version (22.04, 9.3, etc.)
	PlatformVersion string `json:"platformVersion"`

	// KernelVersion is the kernel version string
	KernelVersion string `json:"kernelVersion"`

	// KernelArch is the kernel architecture (x86_64, aarch64)
	KernelArch string `json:"kernelArch"`

	// Arch is the Go architecture (amd64, arm64) - matches binary arch
	Arch string `json:"arch"`

	// Hostname is the system hostname
	Hostname string `json:"hostname"`

	// HostID is the machine's unique identifier (UUID)
	HostID string `json:"hostId"`

	// Virtualization info
	VirtualizationSystem string `json:"virtSystem,omitempty"` // kvm, docker, vmware, etc.
	VirtualizationRole   string `json:"virtRole,omitempty"`   // guest or host

	// CPU information
	CPUModel   string `json:"cpuModel"`   // Intel(R) Core(TM) i7-9750H, etc.
	CPUCores   int    `json:"cpuCores"`   // Physical core count
	CPUThreads int    `json:"cpuThreads"` // Logical CPU count (with hyperthreading)

	// Memory information
	MemoryTotal uint64 `json:"memoryTotal"` // Total RAM in bytes

	// Boot time as Unix timestamp
	BootTime uint64 `json:"bootTime"`

	// Number of running processes
	ProcessCount uint64 `json:"processCount"`

	// Agent version (populated by caller)
	AgentVersion string `json:"agentVersion,omitempty"`
}

// Collect gathers system information from the host.
// This should be called at agent startup and periodically (e.g., hourly)
// to detect system changes like OS upgrades.
func Collect(ctx context.Context) (*SystemInfo, error) {
	info := &SystemInfo{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}

	// Get host info (OS, platform, kernel, virtualization)
	hostInfo, err := host.InfoWithContext(ctx)
	if err == nil {
		info.Platform = hostInfo.Platform
		info.PlatformFamily = hostInfo.PlatformFamily
		info.PlatformVersion = hostInfo.PlatformVersion
		info.KernelVersion = hostInfo.KernelVersion
		info.KernelArch = hostInfo.KernelArch
		info.Hostname = hostInfo.Hostname
		info.HostID = hostInfo.HostID
		info.VirtualizationSystem = hostInfo.VirtualizationSystem
		info.VirtualizationRole = hostInfo.VirtualizationRole
		info.BootTime = hostInfo.BootTime
		info.ProcessCount = hostInfo.Procs
	}

	// Check context
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Get CPU info
	cpuInfo, err := cpu.InfoWithContext(ctx)
	if err == nil && len(cpuInfo) > 0 {
		info.CPUModel = cpuInfo[0].ModelName
		info.CPUCores = int(cpuInfo[0].Cores)
	}

	// Get logical CPU count (threads)
	cpuCount, err := cpu.CountsWithContext(ctx, true)
	if err == nil {
		info.CPUThreads = cpuCount
	}

	// Check context
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Get memory info
	memInfo, err := mem.VirtualMemoryWithContext(ctx)
	if err == nil {
		info.MemoryTotal = memInfo.Total
	}

	return info, nil
}
