// Shared wire types for Kilo CLI JSON message discrimination.

package kilo

// typeProbe extracts the type discriminator from a JSON object.
type typeProbe struct {
	Type string `json:"type"`
}
