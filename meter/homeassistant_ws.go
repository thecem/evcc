package meter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
	"github.com/evcc-io/evcc/util/transport"
)

func init() {
	registry.AddCtx("homeassistant-ws", NewHomeAssistantWSFromConfig)
}

// HomeAssistantWS meter implementation with direct WebSocket
type HomeAssistantWS struct {
	log          *util.Logger
	uri          string
	token        string
	powerEntity  string
	energyEntity string
	scale        float64
	insecure     bool
	powerVal     *util.Monitor[string]
	energyVal    *util.Monitor[string]
	conn         *websocket.Conn
	msgID        int64
	mu           sync.Mutex
}

type HAMessage struct {
	ID      int64       `json:"id,omitempty"`
	Type    string      `json:"type"`
	Success bool        `json:"success,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type HAAuthMessage struct {
	Type        string `json:"type"`
	AccessToken string `json:"access_token"`
}

type HASubscribeMessage struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	EventType string `json:"event_type"`
}

type HAEvent struct {
	Type  string `json:"type"`
	Event struct {
		EventType string `json:"event_type"`
		Data      struct {
			EntityID string `json:"entity_id"`
			NewState struct {
				State string `json:"state"`
			} `json:"new_state"`
		} `json:"data"`
	} `json:"event"`
}

// NewHomeAssistantWSFromConfig creates a HomeAssistant WebSocket meter from config
func NewHomeAssistantWSFromConfig(ctx context.Context, other map[string]interface{}) (api.Meter, error) {
	cc := struct {
		URI      string
		Token    string
		Entity   string // entity parameter from template
		EntityID string `mapstructure:"entity_id"` // backward compatibility
		Power    string // power sensor entity ID
		Energy   string // energy sensor entity ID (optional)
		Scale    float64
		Timeout  time.Duration
		Insecure bool
	}{
		Scale:   1,
		Timeout: 30 * time.Second,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	// Use entity parameter (from template) or entity_id (backward compatibility) or power
	powerEntity := cc.Power
	if powerEntity == "" {
		powerEntity = cc.Entity
	}
	if powerEntity == "" {
		powerEntity = cc.EntityID
	}

	if cc.URI == "" {
		return nil, fmt.Errorf("missing uri")
	}
	if cc.Token == "" {
		return nil, fmt.Errorf("missing token")
	}
	if powerEntity == "" {
		return nil, fmt.Errorf("missing power sensor entity")
	}

	log := util.NewLogger("homeassistant-ws")

	// Use URI as provided (template should include full WebSocket endpoint)
	wsURI := cc.URI
	// Convert HTTP to WS if needed but don't add /api/websocket if already present
	if wsURI[:4] == "http" {
		wsURI = "ws" + wsURI[4:]
	}
	// Only add /api/websocket if not already present
	if len(wsURI) < 14 || wsURI[len(wsURI)-14:] != "/api/websocket" {
		wsURI += "/api/websocket"
	}

	m := &HomeAssistantWS{
		log:          log,
		uri:          wsURI,
		token:        cc.Token,
		powerEntity:  powerEntity,
		energyEntity: cc.Energy,
		scale:        cc.Scale,
		insecure:     cc.Insecure,
		powerVal:     util.NewMonitor[string](cc.Timeout),
	}

	// Set initial placeholder value so meter is available immediately
	m.powerVal.Set("0")
	log.DEBUG.Printf("🏁 Initial power value set to 0 (will update with real data)")

	if cc.Energy != "" {
		m.energyVal = util.NewMonitor[string](cc.Timeout)
		m.energyVal.Set("0")
	}

	// Start WebSocket connection in background
	errC := make(chan error, 1)
	go m.run(errC)

	// Don't wait for WebSocket connection since we have immediate value
	// The connection will establish in background and provide real-time updates

	log.DEBUG.Printf("✅ WebSocket meter ready for power: %s (will connect in background)", powerEntity)
	return m, nil
}

func (m *HomeAssistantWS) nextMsgID() int64 {
	return atomic.AddInt64(&m.msgID, 1)
}

func (m *HomeAssistantWS) run(errC chan error) {
	var once sync.Once
	retryDelay := 5 * time.Second

	for {
		if err := m.connect(); err != nil {
			once.Do(func() { errC <- err })
			m.log.ERROR.Println("connect:", err)
			time.Sleep(retryDelay)
			continue
		}

		if err := m.authenticate(); err != nil {
			once.Do(func() { errC <- err })
			m.log.ERROR.Println("auth:", err)
			m.disconnect()
			time.Sleep(retryDelay)
			continue
		}

		if err := m.subscribe(); err != nil {
			once.Do(func() { errC <- err })
			m.log.ERROR.Println("subscribe:", err)
			m.disconnect()
			time.Sleep(retryDelay)
			continue
		}

		// Connected and subscribed successfully
		once.Do(func() { close(errC) })

		if err := m.listen(); err != nil {
			m.log.ERROR.Println("listen:", err)
			m.disconnect()
			time.Sleep(retryDelay)
		}
	}
}

func (m *HomeAssistantWS) connect() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	opts := &websocket.DialOptions{}
	if m.insecure {
		opts.HTTPClient = &http.Client{
			Transport: request.NewTripper(m.log, transport.Insecure()),
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, m.uri, opts)
	if err != nil {
		return err
	}

	m.conn = conn
	m.log.DEBUG.Printf("🔗 Connected to %s", m.uri)
	return nil
}

func (m *HomeAssistantWS) disconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.conn != nil {
		_ = m.conn.Close(websocket.StatusNormalClosure, "")
		m.conn = nil
	}
}

func (m *HomeAssistantWS) authenticate() error {
	// Read auth_required message
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, data, err := m.conn.Read(ctx)
	if err != nil {
		return err
	}

	var authRequired HAMessage
	if err := json.Unmarshal(data, &authRequired); err != nil {
		return err
	}

	if authRequired.Type != "auth_required" {
		return fmt.Errorf("expected auth_required, got %s", authRequired.Type)
	}

	// Send authentication
	auth := HAAuthMessage{
		Type:        "auth",
		AccessToken: m.token,
	}

	authData, err := json.Marshal(auth)
	if err != nil {
		return err
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	if err := m.conn.Write(ctx2, websocket.MessageText, authData); err != nil {
		return err
	}

	// Read auth response
	ctx3, cancel3 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel3()

	_, data, err = m.conn.Read(ctx3)
	if err != nil {
		return err
	}

	var authResponse HAMessage
	if err := json.Unmarshal(data, &authResponse); err != nil {
		return err
	}

	if authResponse.Type == "auth_invalid" {
		return fmt.Errorf("authentication failed: %s", authResponse.Error.Message)
	}

	if authResponse.Type != "auth_ok" {
		return fmt.Errorf("expected auth_ok, got %s", authResponse.Type)
	}

	m.log.DEBUG.Printf("🔐 Authenticated successfully")
	return nil
}

func (m *HomeAssistantWS) subscribe() error {
	subscribe := HASubscribeMessage{
		ID:        m.nextMsgID(),
		Type:      "subscribe_events",
		EventType: "state_changed",
	}

	subscribeData, err := json.Marshal(subscribe)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := m.conn.Write(ctx, websocket.MessageText, subscribeData); err != nil {
		return err
	}

	// Read subscription response
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	_, data, err := m.conn.Read(ctx2)
	if err != nil {
		return err
	}

	var response HAMessage
	if err := json.Unmarshal(data, &response); err != nil {
		return err
	}

	if !response.Success {
		return fmt.Errorf("subscription failed: %s", response.Error.Message)
	}

	m.log.DEBUG.Printf("📡 Subscribed to real-time updates for %s", m.powerEntity)
	return nil
}

func (m *HomeAssistantWS) listen() error {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, data, err := m.conn.Read(ctx)
		cancel()

		if err != nil {
			return err
		}

		var event HAEvent
		if err := json.Unmarshal(data, &event); err != nil {
			continue // Skip malformed messages
		}

		if event.Type == "event" && event.Event.EventType == "state_changed" {
			entityID := event.Event.Data.EntityID
			state := event.Event.Data.NewState.State

			if entityID == m.powerEntity && state != "" {
				m.powerVal.Set(state)
				m.log.DEBUG.Printf("⚡ Power updated: %s = %s", entityID, state)
			}

			if entityID == m.energyEntity && state != "" && m.energyVal != nil {
				m.energyVal.Set(state)
				m.log.DEBUG.Printf("🔋 Energy updated: %s = %s", entityID, state)
			}
		}
	}
}

// CurrentPower implements the api.Meter interface
func (m *HomeAssistantWS) CurrentPower() (float64, error) {
	val, err := m.powerVal.Get()
	if err != nil {
		return 0, err
	}

	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0, err
	}

	return f * m.scale, nil
}

// TotalEnergy implements the api.MeterEnergy interface
func (m *HomeAssistantWS) TotalEnergy() (float64, error) {
	if m.energyVal == nil {
		// Energy is optional - return 0 instead of error
		return 0, nil
	}

	val, err := m.energyVal.Get()
	if err != nil {
		return 0, err
	}

	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0, err
	}

	return f * m.scale, nil
}

var _ api.Meter = (*HomeAssistantWS)(nil)
var _ api.MeterEnergy = (*HomeAssistantWS)(nil)
