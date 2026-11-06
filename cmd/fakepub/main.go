// Command fakepub publishes simulated all-in-one BESS telemetry for testing.
//
// Each device simulates a day: solar follows a sine curve from 06:00 to 18:00,
// load has morning and evening peaks, the battery charges from surplus solar
// and discharges to cover deficits (down to 20% SoC), and the grid covers the rest.
// -speed compresses simulated time so a full day takes minutes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"bess-monitor/internal/config"
)

const (
	cells       = 8     // LiFePO4 cells in series (~25.6 V pack)
	capacityAh  = 100.0 // battery capacity
	maxChargeW  = 1200.0
	maxDischgW  = 1500.0
	minSoC      = 20.0 // stop discharging below this
	ambientC    = 25.0
	acNominalV  = 220.0
	acNominalHz = 50.0
)

type device struct {
	id         string
	clock      time.Duration // simulated time of day
	soc, soh   float64
	solarPeak  float64 // W
	loadBase   float64 // W
	cellOffset [cells]float64
	temps      [2]float64
	faultLeft  int // messages remaining in the current fault
	errCode    int
}

func newDevice(id string) *device {
	d := &device{
		id:        id,
		clock:     6 * time.Hour,
		soc:       40 + rand.Float64()*40,
		soh:       97 + rand.Float64()*3,
		solarPeak: 700 + rand.Float64()*300,
		loadBase:  150 + rand.Float64()*100,
		temps:     [2]float64{ambientC, ambientC},
	}
	for i := range d.cellOffset {
		d.cellOffset[i] = rand.NormFloat64() * 3 // small per-cell imbalance, mV
	}
	return d
}

// powers returns solar, load, battery (+ charging) and grid power in W for the current hour.
func (d *device) powers() (solar, load, bat, grid float64) {
	h := d.clock.Hours()
	if h > 6 && h < 18 {
		clouds := 0.85 + 0.15*rand.Float64()
		solar = d.solarPeak * math.Sin(math.Pi*(h-6)/12) * clouds
	}
	load = d.loadBase + rand.NormFloat64()*20
	if h >= 6 && h < 8 {
		load += 250
	}
	if h >= 18 && h < 22 {
		load += 500
	}
	load = math.Max(50, load)

	if net := solar - load; net > 0 {
		if d.soc < 100 {
			bat = math.Min(net, maxChargeW)
		}
		solar = load + bat // curtail what the battery cannot take
	} else if d.soc > minSoC {
		bat = -math.Min(-net, maxDischgW)
	}
	grid = math.Max(0, load+bat-solar)
	return solar, load, bat, grid
}

// step advances the simulation by dt and returns the payload for this moment.
func (d *device) step(dt time.Duration, faultP float64) map[string]any {
	d.clock = (d.clock + dt) % (24 * time.Hour)
	solar, load, bat, grid := d.powers()

	ocv := cells * (3000 + 3*d.soc) / 1000 // open-circuit pack voltage, V
	current := bat / ocv

	cellMV := make([]int, cells)
	packMV := 0
	for i := range cellMV {
		mv := 3000 + 3*d.soc + d.cellOffset[i] + current*2 + rand.NormFloat64()
		cellMV[i] = int(math.Round(mv))
		packMV += cellMV[i]
	}

	d.soc = clamp(d.soc+current*dt.Hours()/capacityAh*100, 0, 100)
	for i := range d.temps {
		target := ambientC + math.Abs(current)*0.15 + float64(i)*0.3
		d.temps[i] += (target - d.temps[i]) * math.Min(1, dt.Hours()*2)
	}

	status := 0
	if d.faultLeft == 0 && rand.Float64() < faultP {
		d.faultLeft, d.errCode = 3+rand.Intn(8), 1+rand.Intn(20)
	}
	if d.faultLeft > 0 {
		d.faultLeft--
		status = 255
	} else {
		d.errCode = 0
	}

	solarV := 0.0
	if solar > 0 {
		solarV = 26 + rand.Float64()*4
	}
	acV := acNominalV + rand.NormFloat64()*1.5
	hz := round(acNominalHz+rand.NormFloat64()*0.03, 2)

	return map[string]any{
		"device_id": d.id,
		"status":    status,
		"battery": map[string]any{
			"error_code":   d.errCode,
			"soc":          round(d.soc, 1),
			"soh":          round(d.soh, 1),
			"voltage":      round(float64(packMV)/1000, 2),
			"current":      round(current, 2),
			"temperature":  []float64{round(d.temps[0], 1), round(d.temps[1], 1)},
			"cell_voltage": cellMV,
		},
		"solar": map[string]any{"voltage": round(solarV, 1), "current": round(div(solar, solarV), 2), "power": round(solar, 2)},
		"grid":  map[string]any{"voltage": round(acV, 1), "current": round(grid/acV, 2), "power": round(grid, 2), "freq": hz},
		"load":  map[string]any{"voltage": round(acV, 1), "current": round(load/acV, 2), "power": round(load, 2), "freq": hz},
	}
}

// malformed payloads used to exercise validation on the subscriber side.
var malformed = []string{
	`not json`,
	`{"device_id":"x",}`,
	`{"status":0,"battery":{},"solar":{},"grid":{},"load":{}}`,
	`{"device_id":"x","status":7,"battery":{},"solar":{},"grid":{},"load":{}}`,
	`{"device_id":"x","status":0,"battery":{"soc":150},"solar":{},"grid":{},"load":{}}`,
	`{"device_id":"x","status":0,"battery":{},"solar":{},"grid":{}}`,
}

func main() {
	devices := flag.String("devices", "MOS-AIO-30001", "comma-separated device ids")
	interval := flag.Duration("interval", 5*time.Second, "publish interval")
	speed := flag.Float64("speed", 60, "simulated seconds per real second (60 = 1 day in 24 min)")
	faultP := flag.Float64("fault", 0, "probability (0-1) per message of starting a fault")
	badP := flag.Float64("bad", 0, "probability (0-1) per message of sending a malformed payload")
	flag.Parse()

	if err := run(strings.Split(*devices, ","), *interval, *speed, *faultP, *badP); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ids []string, interval time.Duration, speed, faultP, badP float64) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if strings.Contains(cfg.MQTTTopic, "#") || strings.Count(cfg.MQTTTopic, "+") != 1 {
		return fmt.Errorf("MQTT_TOPIC %q must contain exactly one '+' for the device id", cfg.MQTTTopic)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	opts := mqtt.NewClientOptions().
		AddBroker(cfg.MQTTURL).
		SetClientID(fmt.Sprintf("fakepub-%d", os.Getpid())).
		SetUsername(cfg.MQTTUser).
		SetPassword(cfg.MQTTPass).
		SetAutoReconnect(true)
	client := mqtt.NewClient(opts)
	if t := client.Connect(); t.Wait() && t.Error() != nil {
		return fmt.Errorf("mqtt connect: %w", t.Error())
	}
	defer client.Disconnect(250)

	devs := make([]*device, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			devs = append(devs, newDevice(id))
		}
	}
	slog.Info("publishing", "broker", cfg.MQTTURL, "devices", len(devs), "interval", interval, "speed", speed)

	simStep := time.Duration(float64(interval) * speed)
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		for _, d := range devs {
			msg, _ := json.Marshal(d.step(simStep, faultP))
			if rand.Float64() < badP {
				msg = []byte(malformed[rand.Intn(len(malformed))])
			}
			topic := strings.Replace(cfg.MQTTTopic, "+", d.id, 1)
			if t := client.Publish(topic, 1, false, msg); t.WaitTimeout(5*time.Second) && t.Error() != nil {
				slog.Warn("publish failed", "device", d.id, "err", t.Error())
			}
		}
		select {
		case <-ctx.Done():
			slog.Info("stopped")
			return nil
		case <-tick.C:
		}
	}
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func div(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
