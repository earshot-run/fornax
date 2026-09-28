package modelrt

import "github.com/earshot-run/fornax/internal/catalog"

// FitBudget is the memory a model should fit within to run well: VRAM when
// the machine has a separate, smaller pool, else total RAM (Apple Silicon
// shares one). 0 when the machine's memory is unknown.
func FitBudget() int64 {
	ram := MemoryBytes()
	if vram := VRAMBytes(); vram > 0 && vram < ram {
		return vram
	}
	return ram
}

// How a model fits this machine: comfortably when it fits in VRAM (or in RAM
// when there is no separate VRAM — Apple Silicon shares one pool), tight when
// it only fits in RAM and would offload to the CPU, and won't fit beyond
// that. A machine whose VRAM is unknown falls back to RAM alone.
func Fit(weightBytes int64) catalog.Fit {
	ram := MemoryBytes()
	vram := VRAMBytes()
	if vram > 0 && vram < ram {
		switch {
		case catalog.FitFor(weightBytes, vram) == catalog.Fits:
			return catalog.Fits
		case catalog.FitFor(weightBytes, ram) == catalog.Wont:
			return catalog.Wont
		default:
			return catalog.Tight
		}
	}
	return catalog.FitFor(weightBytes, ram)
}
