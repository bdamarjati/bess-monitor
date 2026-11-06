package ingest

import (
	"strings"
	"testing"
	"time"
)

const sample = `{
  "device_id": "MOS-AIO-30001",
  "status": 0,
  "battery": {
    "error_code": 0, "soc": 88, "soh": 99, "voltage": 24.8, "current": 1.2,
    "temperature": [28.1, 28.2],
    "cell_voltage": [3210, 3209, 3211, 3210, 3210, 3212, 3208]
  },
  "solar": { "voltage": 26.2, "current": 5.2, "power": 136.24 },
  "grid":  { "voltage": 220.3, "current": 1.2, "power": 264.36, "freq": 50.1 },
  "load":  { "voltage": 220.3, "current": 1.2, "power": 264.36, "freq": 50.1 }
}`

var now = time.Unix(1730822400, 0)

func TestDecodeSample(t *testing.T) {
	r, err := Decode([]byte(sample), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.DeviceID != "MOS-AIO-30001" || r.Status != StatusOK || r.TS != now.Unix() {
		t.Fatalf("bad header fields: %+v", r)
	}
	if *r.Battery.SoC != 88 || *r.Battery.Current != 1.2 || *r.Grid.Freq != 50.1 || *r.Solar.Power != 136.24 {
		t.Fatalf("bad values: %+v", r)
	}
	if len(r.Battery.Temperature) != 2 || len(r.Battery.CellVoltage) != 7 {
		t.Fatalf("bad arrays: %+v", r.Battery)
	}
	lo, hi, ok := r.Battery.CellMinMax()
	if !ok || lo != 3208 || hi != 3212 {
		t.Fatalf("cell min/max = %d,%d,%v", lo, hi, ok)
	}
}

func TestDecodeOptionalFieldsMissing(t *testing.T) {
	msg := `{"device_id":"d1","status":255,"battery":{},"solar":{},"grid":{},"load":{}}`
	r, err := Decode([]byte(msg), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Status != StatusFault || r.Battery.SoC != nil || r.Grid.Freq != nil || r.Battery.ErrorCode != nil {
		t.Fatalf("expected nil optional fields: %+v", r)
	}
	if _, _, ok := r.Battery.CellMinMax(); ok {
		t.Fatal("CellMinMax should report no cells")
	}
}

func TestDecodeInvalid(t *testing.T) {
	cases := map[string]string{
		"empty":          ``,
		"not json":       `hello`,
		"trailing comma": `{"device_id":"d1",}`,
		"missing device": strings.Replace(sample, `"device_id": "MOS-AIO-30001",`, ``, 1),
		"empty device":   strings.Replace(sample, `"MOS-AIO-30001"`, `""`, 1),
		"missing status": strings.Replace(sample, `"status": 0,`, ``, 1),
		"bad status":     strings.Replace(sample, `"status": 0`, `"status": 7`, 1),
		"status string":  strings.Replace(sample, `"status": 0`, `"status": "ok"`, 1),
		"missing grid":   `{"device_id":"d1","status":0,"battery":{},"solar":{},"load":{}}`,
		"battery null":   `{"device_id":"d1","status":0,"battery":null,"solar":{},"grid":{},"load":{}}`,
		"soc too high":   strings.Replace(sample, `"soc": 88`, `"soc": 101`, 1),
		"soc negative":   strings.Replace(sample, `"soc": 88`, `"soc": -1`, 1),
		"soh too high":   strings.Replace(sample, `"soh": 99`, `"soh": 150`, 1),
		"NaN token":      strings.Replace(sample, `"voltage": 24.8`, `"voltage": NaN`, 1),
		"overflow":       strings.Replace(sample, `"voltage": 24.8`, `"voltage": 1e999`, 1),
		"bad array":      strings.Replace(sample, `[28.1, 28.2]`, `["hot"]`, 1),
	}
	for name, msg := range cases {
		if _, err := Decode([]byte(msg), now); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
