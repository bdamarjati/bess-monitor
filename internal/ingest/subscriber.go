package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type Options struct {
	URL      string
	User     string
	Pass     string
	Topic    string
	ClientID string
}

// Run connects to the broker, subscribes, and calls handle for every valid reading.
// Malformed messages are logged and dropped. Run blocks until ctx is cancelled.
func Run(ctx context.Context, o Options, handle func(Reading)) error {
	onMessage := func(_ mqtt.Client, m mqtt.Message) {
		// Stamp at receive time; the payload carries no timestamp.
		r, err := Decode(m.Payload(), time.Now())
		if err != nil {
			slog.Warn("dropped message", "topic", m.Topic(), "err", err)
			return
		}
		handle(r)
	}

	opts := mqtt.NewClientOptions().
		AddBroker(o.URL).
		SetClientID(o.ClientID).
		SetUsername(o.User).
		SetPassword(o.Pass).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetMaxReconnectInterval(30 * time.Second).
		SetCleanSession(true).
		// Subscribe on every (re)connect so the subscription survives broker restarts.
		SetOnConnectHandler(func(c mqtt.Client) {
			t := c.Subscribe(o.Topic, 1, onMessage)
			t.Wait()
			if err := t.Error(); err != nil {
				slog.Error("subscribe failed", "topic", o.Topic, "err", err)
				return
			}
			slog.Info("mqtt connected", "broker", o.URL, "topic", o.Topic)
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			slog.Warn("mqtt connection lost", "err", err)
		})

	client := mqtt.NewClient(opts)
	// With ConnectRetry the token completes only on success or disconnect, so don't block on it.
	if t := client.Connect(); t.WaitTimeout(10*time.Second) && t.Error() != nil {
		return fmt.Errorf("mqtt connect: %w", t.Error())
	}

	<-ctx.Done()
	client.Disconnect(250)
	return nil
}
