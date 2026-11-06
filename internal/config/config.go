// Package config loads settings from environment variables and an optional .env file.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	MQTTURL   string
	MQTTUser  string
	MQTTPass  string
	MQTTTopic string
	DBPath    string
	HTTPAddr  string

	// Dashboard sign-in. SessionSecret is optional (random per start if empty).
	DashUser      string
	DashPass      string
	SessionSecret string

	// Retention per data tier, in days.
	RetentionRawDays int
	RetentionMinDays int
	RetentionHrDays  int
}

// Load reads .env (if present) and then the environment. Real env vars win over .env.
func Load() (Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return Config{}, err
	}
	c := Config{
		MQTTURL:   get("MQTT_URL", "tcp://127.0.0.1:1883"),
		MQTTUser:  os.Getenv("MQTT_USER"),
		MQTTPass:  os.Getenv("MQTT_PASS"),
		MQTTTopic: get("MQTT_TOPIC", "bess/+/telemetry"),
		DBPath:    get("DB_PATH", "bess.db"),
		HTTPAddr:  get("HTTP_ADDR", "127.0.0.1:8080"),

		DashUser:      os.Getenv("DASH_USER"),
		DashPass:      os.Getenv("DASH_PASS"),
		SessionSecret: os.Getenv("SESSION_SECRET"),
	}
	var err error
	if c.RetentionRawDays, err = days("RETENTION_RAW_DAYS", 7); err != nil {
		return c, err
	}
	if c.RetentionMinDays, err = days("RETENTION_1M_DAYS", 90); err != nil {
		return c, err
	}
	if c.RetentionHrDays, err = days("RETENTION_1H_DAYS", 1825); err != nil {
		return c, err
	}
	return c, nil
}

// days reads a positive whole number of days.
func days(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive whole number of days, got %q", key, v)
	}
	return n, nil
}

func get(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadDotEnv sets KEY=VALUE pairs from path without overriding existing env vars.
// A missing file is not an error.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}
