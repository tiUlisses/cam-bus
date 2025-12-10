// internal/mqttclient/mqttclient.go
package mqttclient

import (
	"fmt"
	"os"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type Client struct {
    client mqtt.Client
}

type Config struct {
    Host     string
    Port     int
    Username string
    Password string
    ClientID string
}

func NewClientFromEnv(defaultClientID string) (*Client, error) {
    host := getenv("MQTT_HOST", "localhost")
    port := getenvInt("MQTT_PORT", 1883)
    user := os.Getenv("MQTT_USERNAME")
    pass := os.Getenv("MQTT_PASSWORD")

    cfg := Config{
        Host:     host,
        Port:     port,
        Username: user,
        Password: pass,
        ClientID: getenv("MQTT_CLIENT_ID", defaultClientID),
    }

    return NewClient(cfg)
}

func NewClient(cfg Config) (*Client, error) {
    broker := fmt.Sprintf("tcp://%s:%d", cfg.Host, cfg.Port)

    opts := mqtt.NewClientOptions()
    opts.AddBroker(broker)
    opts.SetClientID(cfg.ClientID)
    opts.SetCleanSession(true)
    opts.SetAutoReconnect(true)
    opts.SetConnectTimeout(5 * time.Second)
    opts.SetKeepAlive(30 * time.Second)

    if cfg.Username != "" {
        opts.SetUsername(cfg.Username)
        opts.SetPassword(cfg.Password)
    }

    cli := mqtt.NewClient(opts)
    token := cli.Connect()
    if ok := token.WaitTimeout(10 * time.Second); !ok {
        return nil, fmt.Errorf("mqtt connect timeout")
    }
    if err := token.Error(); err != nil {
        return nil, fmt.Errorf("mqtt connect error: %w", err)
    }

    return &Client{client: cli}, nil
}

func (c *Client) Publish(topic string, qos byte, retained bool, payload []byte) error {
    token := c.client.Publish(topic, qos, retained, payload)
    token.Wait()
    return token.Error()
}

func (c *Client) Subscribe(topic string, qos byte, handler func(topic string, payload []byte)) error {
    token := c.client.Subscribe(topic, qos, func(_ mqtt.Client, msg mqtt.Message) {
        handler(msg.Topic(), msg.Payload())
    })
    token.Wait()
    return token.Error()
}

func (c *Client) Close() {
    if c.client != nil && c.client.IsConnected() {
        c.client.Disconnect(250)
    }
}

func getenv(key, def string) string {
    if v := os.Getenv(key); v != "" {
        return v
    }
    return def
}

func getenvInt(key string, def int) int {
    if v := os.Getenv(key); v != "" {
        var x int
        fmt.Sscanf(v, "%d", &x)
        if x > 0 {
            return x
        }
    }
    return def
}
