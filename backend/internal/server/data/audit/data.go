// Package audit defines the persisted OAuth and MCP audit event schema.
package audit

import "time"

// Event records one authorization or tool operation.
type Event struct {
	Time      time.Time `json:"time"`
	UserID    string    `json:"userID,omitempty"`
	Subject   string    `json:"subject,omitempty"`
	Scopes    []string  `json:"scopes,omitempty"`
	Operation string    `json:"operation"`
	Name      string    `json:"name"`
	Args      string    `json:"args,omitempty"`
	Decision  string    `json:"decision"`
	Status    string    `json:"status,omitempty"`
}
