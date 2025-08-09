package vehicle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
	"github.com/evcc-io/evcc/util/transport"
)

type HomeAssistantWSVehicle struct {
	*embed
	log    *util.Logger
	uri    string
	token  string
	conn   *websocket.Conn
	mu     sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc

	// Required sensors
	socEntity string

	// Optional sensors
	rangeEntity      string
	statusEntity     string
	limitSocEntity   string
	odometerEntity   string
	climaterEntity   string
	finishTimeEntity string

	// Optional services
	startScript  string
	stopScript   string
	wakeupScript string

	// Current values (cached from WebSocket)
	socVal        *util.Monitor[string]
	rangeVal      *util.Monitor[string]
	statusVal     *util.Monitor[string]
	limitSocVal   *util.Monitor[string]
	odometerVal   *util.Monitor[string]
	climaterVal   *util.Monitor[string]
	finishTimeVal *util.Monitor[string]

	// HTTP client for service calls
	*request.Helper
	insecure bool
}

// Register on startup
func init() {
	registry.Add("homeassistant-ws", NewHomeAssistantWSVehicleFromConfig)
}

// Constructor from YAML config
func NewHomeAssistantWSVehicleFromConfig(other map[string]any) (api.Vehicle, error) {
	var cc struct {
		embed   `mapstructure:",squash"`
		URI     string
		Token   string
		Sensors struct {
			Soc        string // required
			Range      string // optional
			Status     string // optional
			LimitSoc   string // optional
			Odometer   string // optional
			Climater   string // optional
			FinishTime string // optional
		}
		Services struct {
			Start  string `mapstructure:"start_charging"` // script.*  optional
			Stop   string `mapstructure:"stop_charging"`  // script.*  optional
			Wakeup string // script.*  optional
		}
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	switch {
	case cc.URI == "":
		return nil, errors.New("missing uri")
	case cc.Token == "":
		return nil, errors.New("missing token")
	case cc.Sensors.Soc == "":
		return nil, errors.New("missing soc sensor")
	}

	log := util.NewLogger("ha-vehicle-ws").Redact(cc.Token)

	// Convert HTTP to WebSocket URI
	wsURI := cc.URI
	if strings.HasPrefix(wsURI, "http://") {
		wsURI = "ws://" + wsURI[7:]
	} else if strings.HasPrefix(wsURI, "https://") {
		wsURI = "wss://" + wsURI[8:]
	}
	if !strings.HasSuffix(wsURI, "/api/websocket") {
		wsURI = strings.TrimSuffix(wsURI, "/") + "/api/websocket"
	}

	ctx, cancel := context.WithCancel(context.Background())

	res := &HomeAssistantWSVehicle{
		embed:            &cc.embed,
		log:              log,
		uri:              wsURI,
		token:            cc.Token,
		ctx:              ctx,
		cancel:           cancel,
		socEntity:        cc.Sensors.Soc,
		rangeEntity:      cc.Sensors.Range,
		statusEntity:     cc.Sensors.Status,
		limitSocEntity:   cc.Sensors.LimitSoc,
		odometerEntity:   cc.Sensors.Odometer,
		climaterEntity:   cc.Sensors.Climater,
		finishTimeEntity: cc.Sensors.FinishTime,
		startScript:      cc.Services.Start,
		stopScript:       cc.Services.Stop,
		wakeupScript:     cc.Services.Wakeup,
		Helper:           request.NewHelper(log),
	}

	// Setup HTTP client for service calls
	res.Client.Transport = &transport.Decorator{
		Base: res.Client.Transport,
		Decorator: transport.DecorateHeaders(map[string]string{
			"Authorization": "Bearer " + cc.Token,
		}),
	}

	// Initialize monitors for all sensors (using 30s timeout like meter)
	timeout := 30 * time.Second
	res.socVal = util.NewMonitor[string](timeout)
	if res.rangeEntity != "" {
		res.rangeVal = util.NewMonitor[string](timeout)
	}
	if res.statusEntity != "" {
		res.statusVal = util.NewMonitor[string](timeout)
	}
	if res.limitSocEntity != "" {
		res.limitSocVal = util.NewMonitor[string](timeout)
	}
	if res.odometerEntity != "" {
		res.odometerVal = util.NewMonitor[string](timeout)
	}
	if res.climaterEntity != "" {
		res.climaterVal = util.NewMonitor[string](timeout)
	}
	if res.finishTimeEntity != "" {
		res.finishTimeVal = util.NewMonitor[string](timeout)
	}

	// Fetch initial values via HTTP API to avoid timeout on first access
	go res.fetchInitialValues()

	// Start WebSocket connection in background
	go res.run()

	// Prepare optional feature functions
	var (
		limitSoc     func() (int64, error)
		status       func() (api.ChargeStatus, error)
		rng          func() (int64, error)
		odo          func() (float64, error)
		climater     func() (bool, error)
		finish       func() (time.Time, error)
		chargeEnable func(bool) error
		wakeup       func() error
	)

	if res.limitSocEntity != "" {
		limitSoc = func() (int64, error) { return res.getIntSensor(res.limitSocVal) }
	}
	if res.statusEntity != "" {
		status = func() (api.ChargeStatus, error) { return res.status() }
	}
	if res.rangeEntity != "" {
		rng = func() (int64, error) { return res.getIntSensor(res.rangeVal) }
	}
	if res.odometerEntity != "" {
		odo = func() (float64, error) { return res.getFloatSensor(res.odometerVal) }
	}
	if res.climaterEntity != "" {
		climater = func() (bool, error) { return res.getBoolSensor(res.climaterVal) }
	}
	if res.finishTimeEntity != "" {
		finish = func() (time.Time, error) { return res.getTimeSensor(res.finishTimeVal) }
	}
	if res.startScript != "" && res.stopScript != "" {
		chargeEnable = func(enable bool) error {
			if enable {
				return res.callScript(res.startScript)
			}
			return res.callScript(res.stopScript)
		}
	}
	if res.wakeupScript != "" {
		wakeup = func() error { return res.callScript(res.wakeupScript) }
	}

	// Decorate all features
	return decorateVehicle(
		res,
		limitSoc,
		status,
		rng,
		odo,
		climater,
		nil, // maxCurrent setter not implemented
		nil, // getMaxCurrent getter not implemented
		finish,
		wakeup,
		chargeEnable,
	), nil
}

// Fetch initial values via HTTP API to populate monitors before WebSocket updates
func (v *HomeAssistantWSVehicle) fetchInitialValues() {
	v.log.DEBUG.Println("🔄 Fetching initial values via HTTP API...")

	// Convert WebSocket URI to HTTP for initial fetch
	httpURI := v.uri
	if strings.HasPrefix(httpURI, "ws://") {
		httpURI = "http://" + httpURI[5:]
	} else if strings.HasPrefix(httpURI, "wss://") {
		httpURI = "https://" + httpURI[6:]
	}
	httpURI = strings.TrimSuffix(httpURI, "/api/websocket")

	// Fetch SoC (required)
	if state, err := v.getStateHTTP(httpURI, v.socEntity); err == nil {
		v.socVal.Set(state)
		v.log.DEBUG.Printf("🔋 Initial SoC: %s = %s%%", v.socEntity, state)
	} else {
		v.log.DEBUG.Printf("Failed to fetch initial SoC: %v", err)
	}

	// Fetch optional values
	if v.rangeEntity != "" && v.rangeVal != nil {
		if state, err := v.getStateHTTP(httpURI, v.rangeEntity); err == nil {
			v.rangeVal.Set(state)
			v.log.DEBUG.Printf("🛣️ Initial range: %s = %s km", v.rangeEntity, state)
		}
	}

	if v.statusEntity != "" && v.statusVal != nil {
		if state, err := v.getStateHTTP(httpURI, v.statusEntity); err == nil {
			v.statusVal.Set(state)
			v.log.DEBUG.Printf("⚡ Initial status: %s = %s", v.statusEntity, state)
		}
	}

	if v.limitSocEntity != "" && v.limitSocVal != nil {
		if state, err := v.getStateHTTP(httpURI, v.limitSocEntity); err == nil {
			v.limitSocVal.Set(state)
			v.log.DEBUG.Printf("🎯 Initial target SoC: %s = %s%%", v.limitSocEntity, state)
		}
	}

	if v.odometerEntity != "" && v.odometerVal != nil {
		if state, err := v.getStateHTTP(httpURI, v.odometerEntity); err == nil {
			v.odometerVal.Set(state)
			v.log.DEBUG.Printf("📏 Initial odometer: %s = %s km", v.odometerEntity, state)
		}
	}

	if v.climaterEntity != "" && v.climaterVal != nil {
		if state, err := v.getStateHTTP(httpURI, v.climaterEntity); err == nil {
			v.climaterVal.Set(state)
			v.log.DEBUG.Printf("🌡️ Initial climater: %s = %s", v.climaterEntity, state)
		}
	}

	if v.finishTimeEntity != "" && v.finishTimeVal != nil {
		if state, err := v.getStateHTTP(httpURI, v.finishTimeEntity); err == nil {
			v.finishTimeVal.Set(state)
			v.log.DEBUG.Printf("⏰ Initial finish time: %s = %s", v.finishTimeEntity, state)
		}
	}

	v.log.DEBUG.Println("✅ Initial values fetched")
}

// HTTP helper to get current state (similar to original homeassistant.go)
func (v *HomeAssistantWSVehicle) getStateHTTP(baseURI, entity string) (string, error) {
	var res struct {
		State string `json:"state"`
	}

	uri := fmt.Sprintf("%s/api/states/%s", baseURI, url.PathEscape(entity))
	if err := v.GetJSON(uri, &res); err != nil {
		return "", err
	}

	if res.State == "unknown" || res.State == "unavailable" {
		return "", api.ErrNotAvailable
	}

	return res.State, nil
}

// Main WebSocket connection loop
func (v *HomeAssistantWSVehicle) run() {
	const retryDelay = 5 * time.Second

	for {
		select {
		case <-v.ctx.Done():
			return
		default:
		}

		v.log.DEBUG.Println("🔗 Connecting to", v.uri)
		if err := v.connect(); err != nil {
			v.log.ERROR.Println("connect:", err)
			v.disconnect()
			time.Sleep(retryDelay)
			continue
		}

		v.log.DEBUG.Println("🔐 Authenticating...")
		if err := v.authenticate(); err != nil {
			v.log.ERROR.Println("authenticate:", err)
			v.disconnect()
			time.Sleep(retryDelay)
			continue
		}

		v.log.DEBUG.Println("📡 Subscribing to entity updates...")
		if err := v.subscribe(); err != nil {
			v.log.ERROR.Println("subscribe:", err)
			v.disconnect()
			time.Sleep(retryDelay)
			continue
		}

		v.log.DEBUG.Println("🚗 Vehicle WebSocket connected and subscribed")

		if err := v.listen(); err != nil {
			v.log.ERROR.Println("listen:", err)
			v.disconnect()
			time.Sleep(retryDelay)
		}
	}
}

func (v *HomeAssistantWSVehicle) connect() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	opts := &websocket.DialOptions{}
	if v.insecure {
		opts.HTTPClient = &http.Client{
			Transport: request.NewTripper(v.log, transport.Insecure()),
		}
	}

	conn, _, err := websocket.Dial(v.ctx, v.uri, opts)
	if err != nil {
		return fmt.Errorf("websocket dial: %w", err)
	}

	v.conn = conn
	return nil
}

func (v *HomeAssistantWSVehicle) disconnect() {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.conn != nil {
		_ = v.conn.Close(websocket.StatusNormalClosure, "")
		v.conn = nil
	}
}

func (v *HomeAssistantWSVehicle) authenticate() error {
	// Wait for auth_required message
	_, authRequired, err := v.conn.Read(v.ctx)
	if err != nil {
		return fmt.Errorf("read auth_required: %w", err)
	}

	var authMsg struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(authRequired, &authMsg); err != nil {
		return fmt.Errorf("decode auth_required: %w", err)
	}

	if authMsg.Type != "auth_required" {
		return fmt.Errorf("expected auth_required, got %s", authMsg.Type)
	}

	// Send authentication
	auth := map[string]interface{}{
		"type":         "auth",
		"access_token": v.token,
	}

	if err := v.conn.Write(v.ctx, websocket.MessageText, mustMarshal(auth)); err != nil {
		return fmt.Errorf("write auth: %w", err)
	}

	// Wait for auth_ok
	_, authOk, err := v.conn.Read(v.ctx)
	if err != nil {
		return fmt.Errorf("read auth_ok: %w", err)
	}

	var authResponse struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(authOk, &authResponse); err != nil {
		return fmt.Errorf("decode auth_ok: %w", err)
	}

	if authResponse.Type != "auth_ok" {
		return fmt.Errorf("authentication failed: %s", authResponse.Type)
	}

	return nil
}

func (v *HomeAssistantWSVehicle) subscribe() error {
	// Subscribe to state_changed events
	subscription := map[string]interface{}{
		"id":         1,
		"type":       "subscribe_events",
		"event_type": "state_changed",
	}

	return v.conn.Write(v.ctx, websocket.MessageText, mustMarshal(subscription))
}

func (v *HomeAssistantWSVehicle) listen() error {
	for {
		select {
		case <-v.ctx.Done():
			return nil
		default:
		}

		_, msg, err := v.conn.Read(v.ctx)
		if err != nil {
			return fmt.Errorf("read message: %w", err)
		}

		var response struct {
			Type  string `json:"type"`
			Event *struct {
				EventType string `json:"event_type"`
				Data      *struct {
					EntityID string `json:"entity_id"`
					NewState *struct {
						State string `json:"state"`
					} `json:"new_state"`
				} `json:"data"`
			} `json:"event"`
		}

		if err := json.Unmarshal(msg, &response); err != nil {
			v.log.DEBUG.Printf("decode message: %v", err)
			continue
		}

		if response.Type == "event" && response.Event != nil &&
			response.Event.EventType == "state_changed" && response.Event.Data != nil {
			v.handleStateChange(response.Event.Data.EntityID, response.Event.Data.NewState)
		}
	}
}

func (v *HomeAssistantWSVehicle) handleStateChange(entityID string, newState *struct {
	State string `json:"state"`
}) {
	if newState == nil {
		return
	}

	state := newState.State
	if state == "unknown" || state == "unavailable" {
		return
	}

	// Update monitors based on entity ID
	switch entityID {
	case v.socEntity:
		v.socVal.Set(state)
		v.log.DEBUG.Printf("🔋 SoC updated: %s = %s%%", entityID, state)
	case v.rangeEntity:
		if v.rangeVal != nil {
			v.rangeVal.Set(state)
			v.log.DEBUG.Printf("🛣️ Range updated: %s = %s km", entityID, state)
		}
	case v.statusEntity:
		if v.statusVal != nil {
			v.statusVal.Set(state)
			v.log.DEBUG.Printf("⚡ Status updated: %s = %s", entityID, state)
		}
	case v.limitSocEntity:
		if v.limitSocVal != nil {
			v.limitSocVal.Set(state)
			v.log.DEBUG.Printf("🎯 Target SoC updated: %s = %s%%", entityID, state)
		}
	case v.odometerEntity:
		if v.odometerVal != nil {
			v.odometerVal.Set(state)
			v.log.DEBUG.Printf("📏 Odometer updated: %s = %s km", entityID, state)
		}
	case v.climaterEntity:
		if v.climaterVal != nil {
			v.climaterVal.Set(state)
			v.log.DEBUG.Printf("🌡️ Climater updated: %s = %s", entityID, state)
		}
	case v.finishTimeEntity:
		if v.finishTimeVal != nil {
			v.finishTimeVal.Set(state)
			v.log.DEBUG.Printf("⏰ Finish time updated: %s = %s", entityID, state)
		}
	}
}

// Required api.Vehicle interface
func (v *HomeAssistantWSVehicle) Soc() (float64, error) {
	return v.getFloatSensor(v.socVal)
}

// Service call helpers (still use HTTP API)
func (v *HomeAssistantWSVehicle) callScript(script string) error {
	// Convert WebSocket URI back to HTTP for service calls
	httpURI := v.uri
	if strings.HasPrefix(httpURI, "ws://") {
		httpURI = "http://" + httpURI[5:]
	} else if strings.HasPrefix(httpURI, "wss://") {
		httpURI = "https://" + httpURI[6:]
	}
	httpURI = strings.TrimSuffix(httpURI, "/api/websocket")

	payload := fmt.Sprintf(`{"entity_id": "%s"}`, script)
	uri := fmt.Sprintf("%s/api/services/script/turn_on", httpURI)
	req, _ := request.New(http.MethodPost, uri, strings.NewReader(payload))
	_, err := v.DoBody(req)
	return err
}

// Generic sensor value parsers
func (v *HomeAssistantWSVehicle) getFloatSensor(monitor *util.Monitor[string]) (float64, error) {
	if monitor == nil {
		return 0, api.ErrNotAvailable
	}

	val, err := monitor.Get()
	if err != nil {
		return 0, err
	}

	return strconv.ParseFloat(val, 64)
}

func (v *HomeAssistantWSVehicle) getIntSensor(monitor *util.Monitor[string]) (int64, error) {
	if monitor == nil {
		return 0, api.ErrNotAvailable
	}

	val, err := monitor.Get()
	if err != nil {
		return 0, err
	}

	return strconv.ParseInt(val, 10, 64)
}

func (v *HomeAssistantWSVehicle) getBoolSensor(monitor *util.Monitor[string]) (bool, error) {
	if monitor == nil {
		return false, api.ErrNotAvailable
	}

	val, err := monitor.Get()
	if err != nil {
		return false, err
	}

	return slices.Contains([]string{"on", "true", "1", "active"}, strings.ToLower(val)), nil
}

func (v *HomeAssistantWSVehicle) getTimeSensor(monitor *util.Monitor[string]) (time.Time, error) {
	if monitor == nil {
		return time.Time{}, api.ErrNotAvailable
	}

	val, err := monitor.Get()
	if err != nil {
		return time.Time{}, err
	}

	if ts, err := strconv.ParseInt(val, 10, 64); err == nil {
		return time.Unix(ts, 0), nil
	}

	return time.Parse(time.RFC3339, val)
}

// status returns evcc charge status
func (v *HomeAssistantWSVehicle) status() (api.ChargeStatus, error) {
	var haStatusMap = map[string]api.ChargeStatus{
		"charging":            api.StatusC,
		"on":                  api.StatusC,
		"true":                api.StatusC,
		"active":              api.StatusC,
		"connected":           api.StatusB,
		"ready":               api.StatusB,
		"plugged":             api.StatusB,
		"disconnected":        api.StatusA,
		"off":                 api.StatusA,
		"none":                api.StatusA,
		"error":               api.StatusA,
		"unavailable":         api.StatusA,
		"unknown":             api.StatusA,
		"notreadyforcharging": api.StatusA,
	}

	if v.statusVal == nil {
		return api.StatusNone, api.ErrNotAvailable
	}

	val, err := v.statusVal.Get()
	if err != nil {
		return api.StatusNone, err
	}

	state := strings.ToLower(val)
	if mapped, ok := haStatusMap[state]; ok {
		return mapped, nil
	}

	return api.StatusNone, errors.New("invalid state: " + val)
}

// Cleanup
func (v *HomeAssistantWSVehicle) Close() {
	if v.cancel != nil {
		v.cancel()
	}
	v.disconnect()
}

// Helper for JSON marshaling
func mustMarshal(v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
