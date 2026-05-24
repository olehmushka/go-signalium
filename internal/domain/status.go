package domain

import (
	"database/sql/driver"
	"fmt"
)

// SignalMessageStatus is the Go binding of the Postgres ENUM
// signalium.signal_message_status. Holding it as a typed string lets the
// exhaustive linter catch missing cases on switches.
type SignalMessageStatus string

// Status values match the Postgres ENUM signalium.signal_message_status; see
// docs/persistence.md for the state machine.
const (
	StatusPending         SignalMessageStatus = "PENDING"
	StatusSending         SignalMessageStatus = "SENDING"
	StatusSent            SignalMessageStatus = "SENT"
	StatusFailed          SignalMessageStatus = "FAILED"
	StatusPermanentFailed SignalMessageStatus = "PERMANENT_FAILED"
	StatusTimedOut        SignalMessageStatus = "TIMED_OUT"
)

// IsTerminal reports whether the status is one the worker will never advance
// from on its own (resend transitions back to PENDING explicitly).
func (s SignalMessageStatus) IsTerminal() bool {
	switch s {
	case StatusSent, StatusPermanentFailed, StatusTimedOut:
		return true
	case StatusPending, StatusSending, StatusFailed:
		return false
	default:
		return false
	}
}

// Value encodes the enum for parameterised SQL. pgx serialises the text
// representation against the ENUM type when search_path includes signalium.
func (s SignalMessageStatus) Value() (driver.Value, error) {
	return string(s), nil
}

// Scan accepts string and []byte payloads returned by pgx for enum columns.
func (s *SignalMessageStatus) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*s = ""
		return nil
	case string:
		*s = SignalMessageStatus(v)
		return nil
	case []byte:
		*s = SignalMessageStatus(v)
		return nil
	default:
		return fmt.Errorf("signal_message_status: cannot scan %T", src)
	}
}
