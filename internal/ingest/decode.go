// Package ingest receives BESS telemetry over MQTT, decodes and validates it.
package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	StatusOK    = 0
	StatusFault = 255
)

// Reading is one validated telemetry message. Pointer fields are optional
// and nil when the device did not send them (stored as NULL).
type Reading struct {
	DeviceID string  `json:"device_id"`
	TS       int64   `json:"ts"` // server receive time, unix seconds UTC
	Status   int     `json:"status"`
	Battery  Battery `json:"battery"`
	Solar    DC      `json:"solar"`
	Grid     AC      `json:"grid"`
	Load     AC      `json:"load"`
}

// Battery current is negative when discharging, positive when charging.
type Battery struct {
	ErrorCode   *int      `json:"error_code"`
	SoC         *float64  `json:"soc"`
	SoH         *float64  `json:"soh"`
	Voltage     *float64  `json:"voltage"`
	Current     *float64  `json:"current"`
	Temperature []float64 `json:"temperature"`  // °C, length varies
	CellVoltage []float64 `json:"cell_voltage"` // mV, length varies
}

type DC struct {
	Voltage *float64 `json:"voltage"`
	Current *float64 `json:"current"`
	Power   *float64 `json:"power"`
}

type AC struct {
	Voltage *float64 `json:"voltage"`
	Current *float64 `json:"current"`
	Power   *float64 `json:"power"`
	Freq    *float64 `json:"freq"`
}

// wire mirrors the payload; pointers let us detect missing required parts.
// Keep all knowledge of the wire format in this file.
type wire struct {
	DeviceID *string  `json:"device_id"`
	Status   *int     `json:"status"`
	Battery  *Battery `json:"battery"`
	Solar    *DC      `json:"solar"`
	Grid     *AC      `json:"grid"`
	Load     *AC      `json:"load"`
}

// Decode parses and validates one payload, stamping it with the receive time.
func Decode(data []byte, received time.Time) (Reading, error) {
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return Reading{}, fmt.Errorf("decode json: %w", err)
	}
	if err := validate(&w); err != nil {
		return Reading{}, err
	}
	return Reading{
		DeviceID: *w.DeviceID,
		TS:       received.UTC().Unix(),
		Status:   *w.Status,
		Battery:  *w.Battery,
		Solar:    *w.Solar,
		Grid:     *w.Grid,
		Load:     *w.Load,
	}, nil
}

func validate(w *wire) error {
	switch {
	case w.DeviceID == nil || *w.DeviceID == "":
		return errors.New("missing device_id")
	case w.Status == nil:
		return errors.New("missing status")
	case w.Battery == nil:
		return errors.New("missing battery")
	case w.Solar == nil:
		return errors.New("missing solar")
	case w.Grid == nil:
		return errors.New("missing grid")
	case w.Load == nil:
		return errors.New("missing load")
	}
	if *w.Status != StatusOK && *w.Status != StatusFault {
		return fmt.Errorf("invalid status %d", *w.Status)
	}

	b := w.Battery
	if err := percent("battery.soc", b.SoC); err != nil {
		return err
	}
	if err := percent("battery.soh", b.SoH); err != nil {
		return err
	}
	nums := []*float64{
		b.Voltage, b.Current,
		w.Solar.Voltage, w.Solar.Current, w.Solar.Power,
		w.Grid.Voltage, w.Grid.Current, w.Grid.Power, w.Grid.Freq,
		w.Load.Voltage, w.Load.Current, w.Load.Power, w.Load.Freq,
	}
	for _, p := range nums {
		if p != nil && !finite(*p) {
			return errors.New("non-finite number")
		}
	}
	for _, arr := range [][]float64{b.Temperature, b.CellVoltage} {
		for _, v := range arr {
			if !finite(v) {
				return errors.New("non-finite number in array")
			}
		}
	}
	return nil
}

func percent(name string, p *float64) error {
	if p == nil {
		return nil
	}
	if !finite(*p) || *p < 0 || *p > 100 {
		return fmt.Errorf("%s out of range: %v", name, *p)
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// CellMinMax returns the lowest and highest cell voltage in mV.
// ok is false when there are no cells.
func (b Battery) CellMinMax() (min, max int, ok bool) {
	if len(b.CellVoltage) == 0 {
		return 0, 0, false
	}
	lo, hi := b.CellVoltage[0], b.CellVoltage[0]
	for _, v := range b.CellVoltage[1:] {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	return int(math.Round(lo)), int(math.Round(hi)), true
}
