package audio

import (
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ARM64Optimizer provides resource optimization for ARM64 processors
type ARM64Optimizer struct {
	config *ARM64OptimizationConfig
	mu     sync.RWMutex
	stats  *ResourceStats
}

// ARM64OptimizationConfig provides tuned settings for ARM64 processors
type ARM64OptimizationConfig struct {
	// CPU optimization
	ThreadLimit          int     // Limit goroutines to avoid context switching overhead
	CPUQuota              float64 // CPU usage quota (0.0-1.0)
	
	// Memory optimization
	BufferReuse           bool    // Reuse memory buffers
	MaxBufferSize         int     // Maximum buffer size to prevent allocation
	GCPercent             int     // Garbage collector tuning
	
	// Audio processing optimization
	AudioProcessingThreads int     // Limit audio processing threads
	PitchShiftQuality     int     // Quality setting for pitch shift (1-3)
	EQComplexity          int     // EQ complexity (1-3)
	
	// Concurrency optimization
	MaxConcurrentSessions int     // Limit concurrent sessions
	ConnectionBacklog     int     // Connection queue limit
	WorkerTimeout        int     // Session timeout in seconds
}

// ResourceStats tracks resource usage for optimization
type ResourceStats struct {
	CPUUsage        float64
	MemoryUsage     uint64
	AllocatableMem  uint64 // total allocatable memory (from cgroup or GOGC limit)
	LastUpdate      time.Time
}

// DefaultARM64Config returns optimized settings for ARM64 processors
func DefaultARM64Config() *ARM64OptimizationConfig {
	return &ARM64OptimizationConfig{
		ThreadLimit:           2,      // Match hardware threads (Cobalt 100 has 2 cores)
		CPUQuota:              0.7,    // Use 70% CPU to leave headroom
		BufferReuse:          true,
		MaxBufferSize:         32768,  // 32KB max buffer (small for ARM64)
		GCPercent:            30,      // Aggressive GC for low memory
		AudioProcessingThreads: 1,    // Single thread for audio processing
		PitchShiftQuality:    2,        // Medium quality (balance performance/quality)
		EQComplexity:         2,        // Medium quality EQ — preserves presence boost
		MaxConcurrentSessions: 2,      // Conservative session limit
		ConnectionBacklog:    5,        // Small connection queue
		WorkerTimeout:       30,        // 30 second timeout
	}
}

// NewARM64Optimizer creates an optimizer for ARM64 processors
func NewARM64Optimizer(config *ARM64OptimizationConfig) *ARM64Optimizer {
	if config == nil {
		config = DefaultARM64Config()
	}
	
	// Only limit GOMAXPROCS on ARM64 Linux (production Cobalt 100)
	if runtime.GOARCH == "arm64" && runtime.GOOS == "linux" {
		runtime.GOMAXPROCS(config.ThreadLimit)
	}
	
	return &ARM64Optimizer{
		config: config,
		stats: &ResourceStats{
			LastUpdate: time.Now(),
		},
	}
}

// OptimizeProcessor applies ARM64-specific optimizations to audio processor
func (opt *ARM64Optimizer) OptimizeProcessor(proc *Processor) {
	opt.mu.Lock()
	defer opt.mu.Unlock()

	// Only apply resource optimizations on ARM64 Linux (Cobalt 100)
	if runtime.GOARCH != "arm64" || runtime.GOOS != "linux" {
		return
	}

	// Reduce processing complexity for ARM64
	proc.config.HarmonicMix = math.Min(proc.config.HarmonicMix, 0.1)
	proc.config.ReverbMix = math.Min(proc.config.ReverbMix, 0.1)
	proc.config.CompressRatio = math.Min(proc.config.CompressRatio, 2.0)
	
	// Simplify EQ for performance
	if opt.config.EQComplexity <= 1 {
		proc.config.HighShelfGain = 0
		proc.config.LowShelfGain = 0
		proc.config.PresenceGain = 0
	}
	
	// Limit buffer sizes
	if proc.config.ReverbMix > 0 {
		proc.config.ReverbRoomSize = math.Min(proc.config.ReverbRoomSize, 0.1)
	}
}

// GetOptimizedConfig returns audio processor config optimized for ARM64
func (opt *ARM64Optimizer) GetOptimizedConfig() Config {
	cfg := DefaultConfig()
	
	// Apply ARM64 optimizations
	cfg.HarmonicMix = math.Min(cfg.HarmonicMix, 0.1)
	cfg.ReverbMix = math.Min(cfg.ReverbMix, 0.1)
	cfg.CompressRatio = math.Min(cfg.CompressRatio, 2.0)
	
	// Simple EQ for performance
	if opt.config.EQComplexity <= 1 {
		cfg.HighShelfGain = 0
		cfg.LowShelfGain = 0
		cfg.PresenceGain = 0
	}
	
	return cfg
}

// readCgroupMemLimit attempts to read the cgroup memory limit (container limit in K8s).
// Returns 0 if unavailable.
func readCgroupMemLimit() uint64 {
	// Try cgroup v2 first
	data, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err == nil {
		val := strings.TrimSpace(string(data))
		if val != "max" {
			if v, err := strconv.ParseUint(val, 10, 64); err == nil {
				return v
			}
		}
	}
	// Fall back to cgroup v1
	data, err = os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")
	if err == nil {
		if v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil {
			return v
		}
	}
	return 0
}

// MonitorResources tracks resource usage and adjusts optimization.
func (opt *ARM64Optimizer) MonitorResources() {
	opt.mu.Lock()
	defer opt.mu.Unlock()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	opt.stats.MemoryUsage = m.Alloc
	if opt.stats.AllocatableMem == 0 {
		if cgLimit := readCgroupMemLimit(); cgLimit > 0 {
			opt.stats.AllocatableMem = cgLimit
		} else {
			// Fall back to total system memory
			opt.stats.AllocatableMem = uint64(runtime.NumCPU()) * 512 * 1024 * 1024 // rough estimate
		}
	}
	opt.stats.LastUpdate = time.Now()
}

// throttleThreshold returns the memory usage threshold (as a fraction of
// AllocatableMem) above which ShouldThrottle returns true.
// Can be configured via ARM64_MEMORY_THROTTLE_PCT env var (default 80).
func (opt *ARM64Optimizer) throttleThreshold() float64 {
	pct := 80.0
	if env := os.Getenv("ARM64_MEMORY_THROTTLE_PCT"); env != "" {
		if v, err := strconv.ParseFloat(env, 64); err == nil && v > 0 && v <= 100 {
			pct = v
		}
	}
	return pct / 100.0
}

// ShouldThrottle determines if processing should be throttled based on memory usage.
// Returns true if Go heap allocation exceeds the configured percentage of
// allocatable memory (cgroup limit or system estimate).
func (opt *ARM64Optimizer) ShouldThrottle() bool {
	opt.mu.RLock()
	defer opt.mu.RUnlock()

	if opt.stats.AllocatableMem == 0 {
		return false
	}
	threshold := opt.throttleThreshold()
	usagePct := float64(opt.stats.MemoryUsage) / float64(opt.stats.AllocatableMem)
	return usagePct >= threshold
}