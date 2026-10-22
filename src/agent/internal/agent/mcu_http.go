package agent

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"aiden-agent/internal/mcu"
)

func (s *Server) handleMCU(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	reply := func(code int, response mcu.Response) { w.WriteHeader(code); _ = json.NewEncoder(w).Encode(response) }
	if !bluetoothControlRequestAllowed(r) {
		reply(403, mcu.Response{Status: "DENIED", Error: "MCU APIs are available only over USB or loopback"})
		return
	}
	op := ""
	allowed := "GET"
	led := ""
	switch r.URL.Path {
	case "/api/mcu/state":
		op = "get_state"
	case "/api/mcu/battery":
		op = "get_battery"
	case "/api/mcu/events":
		op = "events_since"
	case "/api/mcu/wakeup-timer":
		allowed = "GET, PUT, DELETE"
		switch r.Method {
		case "GET":
			op = "get_wakeup_timer"
		case "PUT":
			op = "set_wakeup_timer"
		case "DELETE":
			op = "cancel_wakeup_timer"
		}
	case "/api/mcu/leds/red", "/api/mcu/leds/white":
		allowed = "GET, PUT"
		led = strings.TrimPrefix(r.URL.Path, "/api/mcu/leds/")
		switch r.Method {
		case "GET":
			op = "get_led"
		case "PUT":
			op = "set_led"
		}
	default:
		reply(404, mcu.Response{Status: "INVALID_ARGUMENT", Error: "unknown MCU endpoint"})
		return
	}
	if op == "" || (allowed == "GET" && r.Method != "GET") {
		w.Header().Set("Allow", allowed)
		reply(405, mcu.Response{Status: "INVALID_ARGUMENT", Error: "method not allowed"})
		return
	}
	request := mcu.Request{Op: op, LED: led}
	if r.Method == "PUT" || r.Method == "DELETE" {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16*1024))
		if err != nil {
			reply(400, mcu.Response{Status: "INVALID_ARGUMENT", Error: "invalid or oversized JSON request"})
			return
		}
		request, err = mcu.DecodeRequest(data)
		if err != nil {
			reply(400, mcu.Response{Status: "INVALID_ARGUMENT", Error: err.Error()})
			return
		}
		if (request.Op != "" && request.Op != op) || (request.LED != "" && request.LED != led) {
			reply(400, mcu.Response{Status: "INVALID_ARGUMENT", Error: "body conflicts with endpoint"})
			return
		}
		request.Op = op
		request.LED = led
	}
	if op == "events_since" {
		q := r.URL.Query()
		request.Since = q.Get("since")
		request.Generation = q.Get("generation")
		request.Limit = 50
		request.WaitMS = 25000
		for key, target := range map[string]*int{"limit": &request.Limit, "wait_ms": &request.WaitMS} {
			if raw, ok := q[key]; ok {
				if len(raw) != 1 {
					reply(400, mcu.Response{Status: "INVALID_ARGUMENT", Error: "duplicate query parameter"})
					return
				}
				v, err := strconv.Atoi(raw[0])
				if err != nil {
					reply(400, mcu.Response{Status: "INVALID_ARGUMENT", Error: "invalid " + key})
					return
				}
				*target = v
			}
		}
		if request.Limit < 1 || request.Limit > 100 || request.WaitMS < 0 || request.WaitMS > 25000 {
			reply(400, mcu.Response{Status: "INVALID_ARGUMENT", Error: "limit or wait_ms out of range"})
			return
		}
	}
	do := s.mcuRequest
	if do == nil {
		client := mcu.Client{Socket: strings.TrimSpace(os.Getenv("AIDEN_MCU_SERVICE_SOCKET"))}
		do = client.Do
	}
	response, err := do(r.Context(), request)
	if err != nil {
		var api *mcu.APIError
		if errors.As(err, &api) {
			response = mcu.Response{Status: api.Status, Error: api.Message, MCUStatus: api.MCUStatus, UnknownOutcome: api.UnknownOutcome}
		} else {
			response = mcu.Response{Status: "SERVICE_UNAVAILABLE", Error: "MCU service unavailable"}
		}
	}
	status := map[string]int{"OK": 200, "INVALID_ARGUMENT": 400, "DENIED": 403, "CONFLICT": 409, "UNSUPPORTED": 501, "BUSY": 503, "NOT_READY": 503, "SERVICE_UNAVAILABLE": 503, "TIMEOUT": 504, "PROTOCOL_ERROR": 502, "INTERNAL_ERROR": 500}[response.Status]
	if status == 0 {
		status = 500
	}
	reply(status, response)
}
