package observability

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime/metrics"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/semconv/v1.43.0/containerconv"
	"go.opentelemetry.io/otel/semconv/v1.43.0/dbconv"
	"go.opentelemetry.io/otel/semconv/v1.43.0/goconv"
	"go.opentelemetry.io/otel/semconv/v1.43.0/processconv"
)

const (
	cgroupRoot            = "/sys/fs/cgroup/"
	microsecondsPerSecond = 1e6
)

const (
	sampleGoroutines = iota
	sampleProcessors
	sampleStackMemory
	sampleTotalMemory
	sampleReleasedMemory
	sampleMemoryLimit
	sampleHeapGoal
	sampleAllocatedBytes
	sampleAllocatedObjects
	sampleGCCycles
	sampleCPUUser
	sampleCPUGC
	sampleCPUScavenge
	sampleCPUIdle
	sampleGOGC
	sampleCount
)

var sampleNames = [sampleCount]string{
	sampleGoroutines:       "/sched/goroutines:goroutines",
	sampleProcessors:       "/sched/gomaxprocs:threads",
	sampleStackMemory:      "/memory/classes/heap/stacks:bytes",
	sampleTotalMemory:      "/memory/classes/total:bytes",
	sampleReleasedMemory:   "/memory/classes/heap/released:bytes",
	sampleMemoryLimit:      "/gc/gomemlimit:bytes",
	sampleHeapGoal:         "/gc/heap/goal:bytes",
	sampleAllocatedBytes:   "/gc/heap/allocs:bytes",
	sampleAllocatedObjects: "/gc/heap/allocs:objects",
	sampleGCCycles:         "/gc/cycles/total:gc-cycles",
	sampleCPUUser:          "/cpu/classes/user:cpu-seconds",
	sampleCPUGC:            "/cpu/classes/gc/total:cpu-seconds",
	sampleCPUScavenge:      "/cpu/classes/scavenge/total:cpu-seconds",
	sampleCPUIdle:          "/cpu/classes/idle:cpu-seconds",
	sampleGOGC:             "/gc/gogc:percent",
}

type runtimeInstruments struct {
	goroutines    goconv.GoroutineCount
	processors    goconv.ProcessorLimit
	goMemoryUsed  goconv.MemoryUsed
	goMemoryLimit goconv.MemoryLimit
	goHeapGoal    goconv.MemoryGCGoal
	goAllocated   goconv.MemoryAllocated
	goAllocations goconv.MemoryAllocations
	goGCCycles    goconv.MemoryGCCyclesObservable
	goCPUTime     goconv.CPUTimeObservable
	goGOGC        goconv.ConfigGogc

	processCPUTime     processconv.CPUTime
	processMemory      processconv.MemoryUsageObservable
	processThreads     processconv.ThreadCountObservable
	processDescriptors processconv.UnixFileDescriptorCountObservable
	processUptime      processconv.UptimeObservable
	processStarted     time.Time

	containerCPUTime    containerconv.CPUTimeObservable
	containerMemory     containerconv.MemoryUsageObservable
	containerMemoryMax  metric.Int64ObservableUpDownCounter
	containerProcesses  metric.Int64ObservableUpDownCounter
	containerProcessMax metric.Int64ObservableUpDownCounter
}

func registerRuntimeMetrics(meter metric.Meter) error {
	var errs []error
	goroutines, err := goconv.NewGoroutineCount(meter)
	errs = append(errs, err)
	processors, err := goconv.NewProcessorLimit(meter)
	errs = append(errs, err)
	goMemoryUsed, err := goconv.NewMemoryUsed(meter)
	errs = append(errs, err)
	goMemoryLimit, err := goconv.NewMemoryLimit(meter)
	errs = append(errs, err)
	goHeapGoal, err := goconv.NewMemoryGCGoal(meter)
	errs = append(errs, err)
	goAllocated, err := goconv.NewMemoryAllocated(meter)
	errs = append(errs, err)
	goAllocations, err := goconv.NewMemoryAllocations(meter)
	errs = append(errs, err)
	goGCCycles, err := goconv.NewMemoryGCCyclesObservable(meter)
	errs = append(errs, err)
	goCPUTime, err := goconv.NewCPUTimeObservable(meter)
	errs = append(errs, err)
	goGOGC, err := goconv.NewConfigGogc(meter)
	errs = append(errs, err)
	processCPUTime, err := processconv.NewCPUTime(meter)
	errs = append(errs, err)
	processMemory, err := processconv.NewMemoryUsageObservable(meter)
	errs = append(errs, err)
	processThreads, err := processconv.NewThreadCountObservable(meter)
	errs = append(errs, err)
	processDescriptors, err := processconv.NewUnixFileDescriptorCountObservable(meter)
	errs = append(errs, err)
	processUptime, err := processconv.NewUptimeObservable(meter)
	errs = append(errs, err)
	containerCPUTime, err := containerconv.NewCPUTimeObservable(meter)
	errs = append(errs, err)
	containerMemory, err := containerconv.NewMemoryUsageObservable(meter)
	errs = append(errs, err)
	containerMemoryMax, err := meter.Int64ObservableUpDownCounter(
		"home_agent.container.memory.limit",
		metric.WithDescription("Memory limit of the container cgroup."),
		metric.WithUnit("By"),
	)
	errs = append(errs, err)
	containerProcesses, err := meter.Int64ObservableUpDownCounter(
		"home_agent.container.pids.count",
		metric.WithDescription("Processes running in the container cgroup."),
		metric.WithUnit("{process}"),
	)
	errs = append(errs, err)
	containerProcessMax, err := meter.Int64ObservableUpDownCounter(
		"home_agent.container.pids.limit",
		metric.WithDescription("Process limit of the container cgroup."),
		metric.WithUnit("{process}"),
	)
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("create runtime instruments: %w", err)
	}

	instruments := &runtimeInstruments{
		goroutines:          goroutines,
		processors:          processors,
		goMemoryUsed:        goMemoryUsed,
		goMemoryLimit:       goMemoryLimit,
		goHeapGoal:          goHeapGoal,
		goAllocated:         goAllocated,
		goAllocations:       goAllocations,
		goGCCycles:          goGCCycles,
		goCPUTime:           goCPUTime,
		goGOGC:              goGOGC,
		processCPUTime:      processCPUTime,
		processMemory:       processMemory,
		processThreads:      processThreads,
		processDescriptors:  processDescriptors,
		processUptime:       processUptime,
		processStarted:      time.Now(),
		containerCPUTime:    containerCPUTime,
		containerMemory:     containerMemory,
		containerMemoryMax:  containerMemoryMax,
		containerProcesses:  containerProcesses,
		containerProcessMax: containerProcessMax,
	}
	observables := []metric.Observable{
		goroutines.Inst(),
		processors.Inst(),
		goMemoryUsed.Inst(),
		goMemoryLimit.Inst(),
		goHeapGoal.Inst(),
		goAllocated.Inst(),
		goAllocations.Inst(),
		goGCCycles.Inst(),
		goCPUTime.Inst(),
		goGOGC.Inst(),
		processCPUTime.Inst(),
		processMemory.Inst(),
		processThreads.Inst(),
		processDescriptors.Inst(),
		processUptime.Inst(),
		containerCPUTime.Inst(),
		containerMemory.Inst(),
		containerMemoryMax,
		containerProcesses,
		containerProcessMax,
	}
	if _, err := meter.RegisterCallback(instruments.observe, observables...); err != nil {
		return fmt.Errorf("register runtime metric callback: %w", err)
	}
	return nil
}

func (i *runtimeInstruments) observe(_ context.Context, observer metric.Observer) error {
	i.observeGoRuntime(observer)
	i.observeProcess(observer)
	i.observeContainer(observer)
	return nil
}

func (i *runtimeInstruments) observeGoRuntime(observer metric.Observer) {
	samples := make([]metrics.Sample, sampleCount)
	for index := range samples {
		samples[index].Name = sampleNames[index]
	}
	metrics.Read(samples)

	observeUint64(observer, i.goroutines.Inst(), samples[sampleGoroutines])
	observeUint64(observer, i.processors.Inst(), samples[sampleProcessors])
	observeUint64(observer, i.goHeapGoal.Inst(), samples[sampleHeapGoal])
	observeUint64(observer, i.goAllocated.Inst(), samples[sampleAllocatedBytes])
	observeUint64(observer, i.goAllocations.Inst(), samples[sampleAllocatedObjects])
	observeUint64(observer, i.goGCCycles.Inst(), samples[sampleGCCycles])
	observeUint64(observer, i.goGOGC.Inst(), samples[sampleGOGC])

	stacks, stacksRead := uint64Value(samples[sampleStackMemory])
	total, totalRead := uint64Value(samples[sampleTotalMemory])
	released, releasedRead := uint64Value(samples[sampleReleasedMemory])
	if stacksRead {
		observer.ObserveInt64(i.goMemoryUsed.Inst(), int64(stacks),
			metric.WithAttributes(i.goMemoryUsed.AttrMemoryType(goconv.MemoryTypeStack)))
	}
	if stacksRead && totalRead && releasedRead {
		observer.ObserveInt64(i.goMemoryUsed.Inst(), int64(total-released-stacks),
			metric.WithAttributes(i.goMemoryUsed.AttrMemoryType(goconv.MemoryTypeOther)))
	}
	// The runtime reports math.MaxInt64 when GOMEMLIMIT leaves the heap unbounded.
	if limit, read := uint64Value(samples[sampleMemoryLimit]); read && limit != math.MaxInt64 {
		observer.ObserveInt64(i.goMemoryLimit.Inst(), int64(limit))
	}

	cpuClasses := [...]struct {
		sample int
		state  goconv.CPUStateAttr
	}{
		{sampleCPUUser, goconv.CPUStateUser},
		{sampleCPUGC, goconv.CPUStateGC},
		{sampleCPUScavenge, goconv.CPUStateScavenge},
		{sampleCPUIdle, goconv.CPUStateIdle},
	}
	for _, class := range cpuClasses {
		observeFloat64(observer, i.goCPUTime.Inst(), samples[class.sample], i.goCPUTime.AttrCPUState(class.state))
	}
}

func (i *runtimeInstruments) observeProcess(observer metric.Observer) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err == nil {
		observer.ObserveFloat64(i.processCPUTime.Inst(), timevalSeconds(usage.Utime),
			metric.WithAttributes(i.processCPUTime.AttrCPUMode(processconv.CPUModeUser)))
		observer.ObserveFloat64(i.processCPUTime.Inst(), timevalSeconds(usage.Stime),
			metric.WithAttributes(i.processCPUTime.AttrCPUMode(processconv.CPUModeSystem)))
	}
	observer.ObserveFloat64(i.processUptime.Inst(), time.Since(i.processStarted).Seconds())

	// The remaining readings need /proc, which only Linux provides.
	if fields, read := readProcFields("/proc/self/statm"); read && len(fields) > 1 {
		if pages, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			observer.ObserveInt64(i.processMemory.Inst(), pages*int64(os.Getpagesize()))
		}
	}
	if threads, read := readProcValue("/proc/self/status", "Threads:"); read {
		observer.ObserveInt64(i.processThreads.Inst(), threads)
	}
	if entries, err := os.ReadDir("/proc/self/fd"); err == nil {
		observer.ObserveInt64(i.processDescriptors.Inst(), int64(len(entries)))
	}
}

func (i *runtimeInstruments) observeContainer(observer metric.Observer) {
	cpuModes := [...]struct {
		key  string
		mode containerconv.CPUModeAttr
	}{
		{"user_usec", containerconv.CPUModeUser},
		{"system_usec", containerconv.CPUModeSystem},
	}
	for _, cpuMode := range cpuModes {
		if microseconds, read := readProcValue(cgroupRoot+"cpu.stat", cpuMode.key); read {
			observer.ObserveFloat64(i.containerCPUTime.Inst(), float64(microseconds)/microsecondsPerSecond,
				metric.WithAttributes(i.containerCPUTime.AttrCPUMode(cpuMode.mode)))
		}
	}
	if usage, read := readCgroupInt("memory.current"); read {
		observer.ObserveInt64(i.containerMemory.Inst(), usage)
	}
	if limit, read := readCgroupInt("memory.max"); read {
		observer.ObserveInt64(i.containerMemoryMax, limit)
	}
	if processes, read := readCgroupInt("pids.current"); read {
		observer.ObserveInt64(i.containerProcesses, processes)
	}
	if limit, read := readCgroupInt("pids.max"); read {
		observer.ObserveInt64(i.containerProcessMax, limit)
	}
}

func (t *Telemetry) RegisterConnectionPool(name string, stats func() sql.DBStats) error {
	if t == nil {
		return nil
	}
	if stats == nil {
		return fmt.Errorf("connection pool %q statistics reader is required", name)
	}

	var errs []error
	connections, err := dbconv.NewClientConnectionCountObservable(t.meter)
	errs = append(errs, err)
	maxConnections, err := dbconv.NewClientConnectionMaxObservable(t.meter)
	errs = append(errs, err)
	waits, err := t.meter.Int64ObservableCounter(
		"home_agent.store.connection.waits",
		metric.WithDescription("Waits for a free store connection."),
		metric.WithUnit("{wait}"),
	)
	errs = append(errs, err)
	waitTime, err := t.meter.Float64ObservableCounter(
		"home_agent.store.connection.wait_time",
		metric.WithDescription("Time spent waiting for a free store connection."),
		metric.WithUnit("s"),
	)
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("create connection pool instruments: %w", err)
	}

	poolName := connections.AttrClientConnectionPoolName(name)
	pool := metric.WithAttributes(poolName)
	idle := metric.WithAttributes(poolName, connections.AttrClientConnectionState(dbconv.ClientConnectionStateIdle))
	used := metric.WithAttributes(poolName, connections.AttrClientConnectionState(dbconv.ClientConnectionStateUsed))

	callback := func(_ context.Context, observer metric.Observer) error {
		current := stats()
		observer.ObserveInt64(connections.Inst(), int64(current.Idle), idle)
		observer.ObserveInt64(connections.Inst(), int64(current.InUse), used)
		observer.ObserveInt64(maxConnections.Inst(), int64(current.MaxOpenConnections), pool)
		observer.ObserveInt64(waits, current.WaitCount, pool)
		observer.ObserveFloat64(waitTime, current.WaitDuration.Seconds(), pool)
		return nil
	}
	if _, err := t.meter.RegisterCallback(
		callback,
		connections.Inst(),
		maxConnections.Inst(),
		waits,
		waitTime,
	); err != nil {
		return fmt.Errorf("register connection pool metric callback: %w", err)
	}
	return nil
}

func uint64Value(sample metrics.Sample) (uint64, bool) {
	if sample.Value.Kind() != metrics.KindUint64 {
		return 0, false
	}
	return sample.Value.Uint64(), true
}

func observeUint64(observer metric.Observer, instrument metric.Int64Observable, sample metrics.Sample, attributes ...attribute.KeyValue) {
	value, read := uint64Value(sample)
	if !read {
		return
	}
	observer.ObserveInt64(instrument, int64(value), metric.WithAttributes(attributes...))
}

func observeFloat64(observer metric.Observer, instrument metric.Float64Observable, sample metrics.Sample, attributes ...attribute.KeyValue) {
	if sample.Value.Kind() != metrics.KindFloat64 {
		return
	}
	observer.ObserveFloat64(instrument, sample.Value.Float64(), metric.WithAttributes(attributes...))
}

func timevalSeconds(value syscall.Timeval) float64 {
	return float64(value.Nano()) / float64(time.Second)
}

// readProcValue returns the first integer following key in path, which covers
// both "key: value" /proc status files and "key value" cgroup stat files.
func readProcValue(path, key string) (int64, bool) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for line := range strings.Lines(string(content)) {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.TrimSuffix(fields[0], ":") != strings.TrimSuffix(key, ":") {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return value, true
	}
	return 0, false
}

func readProcFields(path string) ([]string, bool) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return strings.Fields(string(content)), true
}

func readCgroupInt(name string) (int64, bool) {
	content, err := os.ReadFile(cgroupRoot + name)
	if err != nil {
		return 0, false
	}
	text := strings.TrimSpace(string(content))
	// cgroup v2 writes "max" instead of a number when the resource is unbounded.
	if text == "max" {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}
