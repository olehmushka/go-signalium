// Package domain holds plain Go types shared by the repo, service, and handler
// layers. Nothing in this package imports infrastructure — sqlc-generated code
// references these types via the overrides declared in sqlc.yaml.
package domain

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// Attachment is a reference to an object already staged in object storage.
// It deliberately mirrors the shape persisted in the signal_messages.attachments
// JSONB column: {bucket, filename, mimeType?, size?}.
type Attachment struct {
	Bucket   string `json:"bucket"`
	Filename string `json:"filename"`
	MimeType string `json:"mimeType,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

// Attachments is the JSONB-backed slice persisted on a signal message. It
// implements sql.Scanner and driver.Valuer so the sqlc-generated repo can
// roundtrip the column without leaking []byte to upper layers.
type Attachments []Attachment

// Value marshals the slice for INSERT/UPDATE statements. A nil slice is encoded
// as an empty JSON array to match the column DEFAULT.
func (a Attachments) Value() (driver.Value, error) {
	if a == nil {
		return []byte("[]"), nil
	}
	b, err := json.Marshal([]Attachment(a))
	if err != nil {
		return nil, fmt.Errorf("marshal attachments: %w", err)
	}
	return b, nil
}

// Scan unmarshals the JSONB payload returned by pgx. nil and an empty payload
// both produce a nil slice.
func (a *Attachments) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	var data []byte
	switch v := src.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return fmt.Errorf("attachments: cannot scan %T", src)
	}
	if len(data) == 0 {
		*a = nil
		return nil
	}
	var out []Attachment
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("unmarshal attachments: %w", err)
	}
	*a = Attachments(out)
	return nil
}
