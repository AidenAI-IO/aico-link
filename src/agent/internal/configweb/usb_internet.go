package configweb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"aico-agent/internal/usbuplink"
)

func newUSBInternetClient(socket string) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

func (s *Server) handleUSBInternet(w http.ResponseWriter, r *http.Request) {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	ip := net.ParseIP(peer)
	local := ip != nil && ip.IsLoopback()
	viaUSB := s.lanAccessRequestFromUSB(r)
	var hostTime time.Time
	var body io.Reader
	if r.Method != http.MethodGet {
		if (!local && !viaUSB) || !sameOrigin(r) {
			writeJSONError(w, http.StatusForbidden, "USB Internet can only be changed over USB")
			return
		}
		var request usbuplink.Request
		if !readJSONBody(w, r, &request) {
			return
		}
		if strings.HasSuffix(r.URL.Path, "/session") {
			// The destination is the actual USB peer or the board's SSH reverse
			// listener. A caller cannot nominate arbitrary proxy addresses.
			request.Peer = peer
			if local {
				request.Peer = "127.0.0.1"
			}
			if r.Method == http.MethodPut && ((request.Host == "mac" && !local) || (request.Host == "android" && local)) {
				writeJSONError(w, http.StatusForbidden, "host does not match the connection")
				return
			}
		}
		if request.UnixTimeMS != 0 && r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/session") {
			hostTime = time.UnixMilli(request.UnixTimeMS)
			if hostTime.Year() < 2024 || hostTime.Year() > 2100 {
				writeJSONError(w, http.StatusBadRequest, "invalid host clock")
				return
			}
		}
		data, _ := json.Marshal(request)
		body = bytes.NewReader(data)
	}
	forward, err := http.NewRequestWithContext(r.Context(), r.Method, "http://usb-internet"+r.URL.Path, body)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "cannot create USB Internet request")
		return
	}
	forward.Header.Set("Content-Type", "application/json")
	response, err := s.usbInternetClient.Do(forward)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "USB Internet service unavailable")
		return
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8192))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "cannot read USB Internet status")
		return
	}
	if response.StatusCode != http.StatusOK {
		writeJSONError(w, response.StatusCode, strings.TrimSpace(string(data)))
		return
	}
	if !hostTime.IsZero() && (time.Since(hostTime) > 2*time.Minute || time.Until(hostTime) > 2*time.Minute) {
		if err := s.usbInternetSetTime(r.Context(), hostTime); err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "cannot synchronize board clock")
			return
		}
	}
	var status usbuplink.Status
	if json.Unmarshal(data, &status) != nil {
		writeJSONError(w, http.StatusBadGateway, "invalid USB Internet status")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, struct {
		usbuplink.Status
		ViaUSB bool `json:"via_usb"`
	}{status, viaUSB || local})
}

func setUSBInternetTime(ctx context.Context, value time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "date", "-u", "-s", fmt.Sprintf("@%d", value.Unix())).Run()
}
