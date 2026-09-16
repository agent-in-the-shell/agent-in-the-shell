package store

import (
	"errors"
	"time"
)

// PortalReportingLocation deliberately supports only the legacy UTC default and
// the Portal's fixed UTC+8 calendar. It does not affect storage or budget windows,
// and needs no host zoneinfo database in the static gateway binary.
func PortalReportingLocation(name string) (*time.Location, error) {
	switch name {
	case "", "UTC":
		return time.UTC, nil
	case "Asia/Taipei":
		return time.FixedZone("Asia/Taipei", 8*60*60), nil
	default:
		return nil, errors.New("unsupported reporting timezone")
	}
}
