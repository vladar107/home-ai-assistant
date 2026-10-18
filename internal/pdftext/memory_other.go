//go:build !linux || race

package pdftext

// limitMemory has no kernel limit to set: macOS does not enforce memory limits, and the race
// detector's shadow memory would trip Linux's. The watchdog is the memory bound there.
func limitMemory(uint64) error {
	return nil
}
