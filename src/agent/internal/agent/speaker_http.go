package agent

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"aico-agent/internal/logging"
)

const speakerEnrollMaxBodyBytes = 16 * 16000 * 2 // 16 seconds PCM16

func (s *Server) handleSpeakerAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/speaker")
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		path = "/"
	}
	gate := NewSpeakerGateFromConfig(s.runtime.ConfigSnapshot())

	switch {
	case path == "/status" && r.Method == http.MethodGet:
		s.writeSpeakerStatus(w, gate)
	case path == "/profiles" && r.Method == http.MethodGet:
		s.writeSpeakerProfiles(w, gate)
	case path == "/enroll" && r.Method == http.MethodPost:
		s.handleSpeakerEnroll(w, r, gate)
	case path == "/enroll/capture" && r.Method == http.MethodPost:
		s.handleSpeakerEnrollCapture(w, r, gate)
	case strings.HasPrefix(path, "/profiles/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(path, "/profiles/")
		if err := gate.Delete(id); err != nil {
			writeSpeakerError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeSpeakerError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleSpeakerEnrollCapture(w http.ResponseWriter, r *http.Request, gate *SpeakerGate) {
	id := NormalizeSpeakerProfileID(r.URL.Query().Get("id"))
	pcm, err := s.captureSpeakerEnrollmentPCM(r.Context())
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "busy") {
			status = http.StatusConflict
		} else if strings.Contains(err.Error(), "start board recording") || strings.Contains(err.Error(), "read board recording") {
			status = http.StatusServiceUnavailable
		}
		logging.Warnf("agent", "speaker", "enroll capture failed id=%s: %v", id, err)
		writeSpeakerError(w, status, err.Error())
		return
	}
	if err := gate.Enroll(id, pcm); err != nil {
		logging.Warnf("agent", "speaker", "enroll embed failed id=%s: %v", id, err)
		writeSpeakerError(w, http.StatusBadRequest, err.Error())
		return
	}
	logging.Infof("agent", "speaker", "enrolled profile id=%s source=board_capture samples=%d", id, len(pcm))
	writeJSON(w, map[string]any{"id": id, "status": "enrolled", "source": "board_capture"})
}

func (s *Server) writeSpeakerStatus(w http.ResponseWriter, gate *SpeakerGate) {
	enabled, count, threshold, err := gate.Status()
	if err != nil {
		writeSpeakerError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg := s.runtime.ConfigSnapshot()
	toolsDir := strings.TrimSpace(cfg.VoiceSpeakerToolsDir)
	if toolsDir == "" {
		toolsDir = DefaultSpeakerToolsDir()
	}
	modelDir := strings.TrimSpace(cfg.VoiceSpeakerModelDir)
	if modelDir == "" {
		modelDir = DefaultSpeakerModelDir()
	}
	storeDir := DefaultSpeakerStoreDir()
	maxProfiles := DefaultSpeakerMaxProfiles()
	if gate.Store != nil {
		storeDir = gate.Store.Dir()
		maxProfiles = gate.Store.MaxProfiles()
	}
	writeJSON(w, map[string]any{
		"gate_enabled":  enabled,
		"profile_count": count,
		"threshold":     threshold,
		"store_dir":     storeDir,
		"max_profiles":  maxProfiles,
		"tools_dir":     toolsDir,
		"model_dir":     modelDir,
	})
}

func (s *Server) writeSpeakerProfiles(w http.ResponseWriter, gate *SpeakerGate) {
	list, err := gate.List()
	if err != nil {
		writeSpeakerError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []SpeakerProfileMeta{}
	}
	writeJSON(w, map[string]any{"profiles": list})
}

func (s *Server) handleSpeakerEnroll(w http.ResponseWriter, r *http.Request, gate *SpeakerGate) {
	id := NormalizeSpeakerProfileID(r.URL.Query().Get("id"))
	body, err := io.ReadAll(io.LimitReader(r.Body, speakerEnrollMaxBodyBytes+1))
	if err != nil {
		writeSpeakerError(w, http.StatusBadRequest, "read body failed")
		return
	}
	if len(body) > speakerEnrollMaxBodyBytes {
		writeSpeakerError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	pcm, err := decodeSpeakerEnrollAudio(body, r.Header.Get("Content-Type"))
	if err != nil {
		writeSpeakerError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := gate.Enroll(id, pcm); err != nil {
		writeSpeakerError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"id": id, "status": "enrolled", "source": "upload"})
}

func writeSpeakerError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": message})
}

func decodeSpeakerEnrollAudio(body []byte, contentType string) ([]int16, error) {
	contentType = strings.ToLower(contentType)
	if strings.Contains(contentType, "wav") || (len(body) >= 12 && string(body[0:4]) == "RIFF" && string(body[8:12]) == "WAVE") {
		return pcm16FromWAV(body)
	}
	if len(body)%2 != 0 || len(body) == 0 {
		return nil, fmt.Errorf("expected raw PCM16 mono body or WAV")
	}
	pcm := make([]int16, len(body)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(body[i*2:]))
	}
	return pcm, nil
}

func pcm16FromWAV(data []byte) ([]int16, error) {
	if len(data) < 44 {
		return nil, fmt.Errorf("wav too short")
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a WAV file")
	}
	offset := 12
	var pcmData []byte
	for offset+8 <= len(data) {
		chunkID := string(data[offset : offset+4])
		chunkSize := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
		offset += 8
		// Use uint64 so large chunk sizes cannot wrap int addition on 32-bit ARM.
		if uint64(offset)+uint64(chunkSize) > uint64(len(data)) {
			return nil, fmt.Errorf("invalid wav chunk")
		}
		size := int(chunkSize)
		switch chunkID {
		case "fmt ":
			if size < 16 {
				return nil, fmt.Errorf("invalid fmt chunk")
			}
			audioFormat := binary.LittleEndian.Uint16(data[offset:])
			channels := binary.LittleEndian.Uint16(data[offset+2:])
			rate := binary.LittleEndian.Uint32(data[offset+4:])
			bits := binary.LittleEndian.Uint16(data[offset+14:])
			if audioFormat != 1 || channels != 1 || rate != speakerSampleRate || bits != 16 {
				return nil, fmt.Errorf("wav must be PCM16 mono %d Hz", speakerSampleRate)
			}
		case "data":
			pcmData = data[offset : offset+size]
		}
		offset += size + (size & 1)
	}
	if len(pcmData) < 2 {
		return nil, fmt.Errorf("wav has no PCM data")
	}
	pcm := make([]int16, len(pcmData)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(pcmData[i*2:]))
	}
	return pcm, nil
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}
