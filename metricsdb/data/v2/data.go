// Package v2 defines version 2 of the persisted daily metrics log.
package v2

import "time"

// Outcome classifies a recorded result.
type Outcome string

// Kind identifies how a measurement aggregates.
type Kind string

// Unit identifies the measurement unit in UCUM notation.
type Unit string

// Record is one measurement as written to the log.
type Record struct {
	Time    time.Time         `json:"time"`
	Name    string            `json:"name"`
	Outcome Outcome           `json:"outcome"`
	Kind    Kind              `json:"kind"`
	Unit    Unit              `json:"unit"`
	Amount  float64           `json:"amount"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Resource names the process that owns a directory of metric files.
type Resource struct {
	Service string `json:"service"`
	Version string `json:"version,omitempty"`
	Host    string `json:"host,omitempty"`
}

// FileHeader identifies the resource and format of one daily metric file.
type FileHeader struct {
	Type     string   `json:"type"`
	Version  int      `json:"version"`
	Resource Resource `json:"resource"`
}
