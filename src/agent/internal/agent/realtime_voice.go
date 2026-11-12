package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"aico-agent/internal/agent/tts"
)

// RealtimeAudioRoute names where a realtime voice session captures and plays
// its audio. The conversation, its transcripts and its Web UI presentation do
// not depend on the route.
type RealtimeAudioRoute string

const (
	// RealtimeAudioRouteDevice uses the board microphone and speaker.
	RealtimeAudioRouteDevice RealtimeAudioRoute = "device"
	// RealtimeAudioRouteWeb uses the microphone and speaker of the browser
	// that started the session.
	RealtimeAudioRouteWeb RealtimeAudioRoute = "web"
)

// RealtimeAudioIO is the media endpoint of one realtime voice session. The
// session reads microphone PCM from Recording and writes response PCM to
// Playback in the formats it negotiated with the provider; the endpoint owns
// any transport between those calls and the actual audio hardware.
type RealtimeAudioIO interface {
	Route() RealtimeAudioRoute
	Recording() AudioRecordingBackend
	Playback() tts.AudioServiceBackend
	// Close releases the endpoint once the session that used it has ended.
	// reason is empty for a normal end.
	Close(reason string)
}

type deviceRealtimeAudio struct {
	recording AudioRecordingBackend
	playback  tts.AudioServiceBackend
}

// NewDeviceRealtimeAudioIO returns the board audio endpoint selected by the
// audio configuration (audio_service or the local host backend).
func NewDeviceRealtimeAudioIO(cfg Config) RealtimeAudioIO {
	service := NewAudioServiceClient(cfg.Audio.SocketOrDefault())
	return deviceRealtimeAudio{
		recording: NewConfiguredAudioRecordingBackend(cfg, service, nil),
		playback:  NewConfiguredAudioPlaybackBackend(cfg, service, nil),
	}
}

func (deviceRealtimeAudio) Route() RealtimeAudioRoute           { return RealtimeAudioRouteDevice }
func (d deviceRealtimeAudio) Recording() AudioRecordingBackend  { return d.recording }
func (d deviceRealtimeAudio) Playback() tts.AudioServiceBackend { return d.playback }
func (deviceRealtimeAudio) Close(string)                        {}

// RealtimeVoiceSession is the control surface of one running realtime voice
// session. The daemon owns the session loop; the Web UI reads its status and
// asks it to stop or to mute the microphone through the Server.
type RealtimeVoiceSession struct {
	route     RealtimeAudioRoute
	audio     RealtimeAudioIO
	startedAt time.Time
	ready     atomic.Bool
	muted     atomic.Bool
	silenced  atomic.Bool
	// rotating is set, under the controller lock, while the session waits for
	// the provider-rotation successor that will replace it.
	rotating bool
	stop     chan struct{}
	stopOnce sync.Once
}

// Route reports where this session captures and plays audio.
func (v *RealtimeVoiceSession) Route() RealtimeAudioRoute {
	if v == nil {
		return RealtimeAudioRouteDevice
	}
	return v.route
}

// MarkReady records that the provider session is connected.
func (v *RealtimeVoiceSession) MarkReady() {
	if v != nil {
		v.ready.Store(true)
	}
}

// Muted reports whether microphone input should be replaced by silence.
func (v *RealtimeVoiceSession) Muted() bool {
	return v != nil && v.muted.Load()
}

// SpeakerMuted reports whether reply audio should be dropped instead of
// played. The conversation, transcripts and tools continue unchanged.
func (v *RealtimeVoiceSession) SpeakerMuted() bool {
	return v != nil && v.silenced.Load()
}

// StopRequested is closed when a user asks to end the session.
func (v *RealtimeVoiceSession) StopRequested() <-chan struct{} {
	if v == nil {
		return nil
	}
	return v.stop
}

// Stopped reports whether a user asked to end the session.
func (v *RealtimeVoiceSession) Stopped() bool {
	if v == nil {
		return false
	}
	select {
	case <-v.stop:
		return true
	default:
		return false
	}
}

func (v *RealtimeVoiceSession) requestStop() {
	v.stopOnce.Do(func() { close(v.stop) })
}

// RealtimeVoiceStatus is the public state of the realtime voice mode.
type RealtimeVoiceStatus struct {
	// Available is true while the agent runs in realtime input mode and can
	// open a voice session.
	Available bool `json:"available"`
	// State is idle, connecting or active.
	State string             `json:"state"`
	Route RealtimeAudioRoute `json:"route,omitempty"`
	Muted bool               `json:"muted"`
	// SpeakerMuted is true while reply audio is dropped instead of played.
	SpeakerMuted bool       `json:"speaker_muted"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	// HTTPSPort is the agent's HTTPS port, or 0 when HTTPS is off. A page
	// opened over plain HTTP uses it to reopen itself where the browser
	// allows microphone access.
	HTTPSPort int `json:"https_port,omitempty"`
}

// realtimeVoiceController joins Web UI requests with the realtime session
// loop. Its zero value is ready for use.
//
// A browser start queues its endpoint in pending and wakes the loop; device
// activations (wake word, MCU, chat, task updates) are tracked by the loop
// itself. The loop asks for a browser endpoint first and opens the device
// only for a device activation, so a browser start withdrawn before the loop
// reaches it opens nothing. Claiming the next session and publishing it
// happen under one lock, so a browser start never slips between them.
type realtimeVoiceController struct {
	mu       sync.Mutex
	activate func()
	pending  *webRealtimeAudio
	active   *RealtimeVoiceSession
}

var (
	errRealtimeVoiceUnavailable = errors.New("realtime voice is not enabled; set the input mode to realtime")
	errRealtimeVoiceBusy        = errors.New("a realtime voice session is already active")
)

// SetRealtimeVoiceActivation installs the callback that wakes the realtime
// session loop. Passing nil marks realtime voice as unavailable and rejects a
// browser session still waiting to start.
func (s *Server) SetRealtimeVoiceActivation(activate func()) {
	if s == nil {
		return
	}
	c := &s.realtimeVoice
	c.mu.Lock()
	c.activate = activate
	pending := c.pending
	if activate == nil {
		c.pending = nil
	}
	c.mu.Unlock()
	if activate == nil && pending != nil {
		pending.Close(errRealtimeVoiceUnavailable.Error())
	}
}

// ClaimRealtimeVoiceSession selects the audio endpoint for the session about
// to open and publishes it as connecting. A browser waiting to start wins;
// otherwise the device is used only when deviceRequested reports a device
// activation. It returns nil when there is nothing to open, for example when
// the only wakeup came from a browser start that was since withdrawn, or when
// the session being rotated was stopped during the handoff. The caller must
// pass a non-nil result to EndRealtimeVoiceSession.
func (s *Server) ClaimRealtimeVoiceSession(cfg Config, deviceRequested bool) *RealtimeVoiceSession {
	if s == nil {
		if !deviceRequested {
			return nil
		}
		audio := NewDeviceRealtimeAudioIO(cfg)
		return &RealtimeVoiceSession{route: audio.Route(), audio: audio, startedAt: time.Now(), stop: make(chan struct{})}
	}
	c := &s.realtimeVoice
	c.mu.Lock()
	defer c.mu.Unlock()
	// A rotated session stays published until this claim replaces it, so a
	// stop pressed during the handoff reached it. Honour that stop here.
	if previous := c.active; previous != nil && previous.rotating {
		c.active = nil
		if previous.Stopped() {
			c.pending = nil
			return nil
		}
	}
	var audio RealtimeAudioIO
	if c.pending != nil && !c.pending.closed() {
		audio = c.pending
	}
	c.pending = nil
	if audio == nil && !deviceRequested {
		return nil
	}
	if audio == nil {
		audio = NewDeviceRealtimeAudioIO(cfg)
	}
	session := &RealtimeVoiceSession{route: audio.Route(), audio: audio, startedAt: time.Now(), stop: make(chan struct{})}
	c.active = session
	return session
}

// RealtimeVoicePending reports whether a browser voice start is waiting for
// the session loop.
func (s *Server) RealtimeVoicePending() bool {
	if s == nil {
		return false
	}
	c := &s.realtimeVoice
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending != nil && !c.pending.closed()
}

// Audio returns the endpoint the session captures and plays through.
func (v *RealtimeVoiceSession) Audio() RealtimeAudioIO {
	if v == nil {
		return nil
	}
	return v.audio
}

// EndRealtimeVoiceSession withdraws session from the published status. When
// rotated is true and the session is a browser session nobody asked to stop,
// its endpoint is kept for the next session and EndRealtimeVoiceSession
// reports true; the caller closes the endpoint otherwise.
//
// The stop check and the requeue happen under the lock that stop requests
// also take, so a stop either lands before the requeue and prevents it, or
// after it and withdraws the requeued start.
func (s *Server) EndRealtimeVoiceSession(session *RealtimeVoiceSession, rotated bool) bool {
	if s == nil || session == nil {
		return false
	}
	c := &s.realtimeVoice
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == session {
		if rotated && !session.Stopped() && c.activate != nil {
			// Keep the session published through the handoff, so a stop
			// pressed before the next session is claimed still reaches it.
			session.rotating = true
		} else {
			c.active = nil
		}
	}
	if !rotated || session.Stopped() || c.pending != nil || c.activate == nil {
		return false
	}
	web, ok := session.audio.(*webRealtimeAudio)
	if !ok || web.closed() {
		return false
	}
	c.pending = web
	return true
}

// ContinueRealtimeVoiceRotation decides, under the controller lock, whether
// a rotated session continues into a successor. It reports false, and
// withdraws the session, when the user stopped it during the handoff. A
// session that continues stays published until the next claim replaces it,
// so a stop that arrives after this call still reaches it there.
func (s *Server) ContinueRealtimeVoiceRotation(session *RealtimeVoiceSession) bool {
	if session == nil {
		return false
	}
	if s == nil {
		return !session.Stopped()
	}
	c := &s.realtimeVoice
	c.mu.Lock()
	defer c.mu.Unlock()
	if !session.Stopped() {
		return true
	}
	if c.active == session {
		c.active = nil
	}
	return false
}

// RealtimeVoiceStatus reports the current realtime voice state.
func (s *Server) RealtimeVoiceStatus() RealtimeVoiceStatus {
	c := &s.realtimeVoice
	c.mu.Lock()
	defer c.mu.Unlock()
	status := RealtimeVoiceStatus{Available: c.activate != nil, State: "idle"}
	switch {
	case c.active != nil:
		status.State = "connecting"
		if c.active.ready.Load() {
			status.State = "active"
		}
		status.Route = c.active.route
		status.Muted = c.active.muted.Load()
		status.SpeakerMuted = c.active.silenced.Load()
		startedAt := c.active.startedAt
		status.StartedAt = &startedAt
	case c.pending != nil:
		status.State = "connecting"
		status.Route = RealtimeAudioRouteWeb
	}
	return status
}

// startWebRealtimeVoice queues a browser endpoint and wakes the session loop.
func (s *Server) startWebRealtimeVoice(web *webRealtimeAudio) error {
	c := &s.realtimeVoice
	c.mu.Lock()
	if c.activate == nil {
		c.mu.Unlock()
		return errRealtimeVoiceUnavailable
	}
	if c.active != nil || c.pending != nil {
		c.mu.Unlock()
		return errRealtimeVoiceBusy
	}
	c.pending = web
	activate := c.activate
	c.mu.Unlock()
	activate()
	return nil
}

// releaseWebRealtimeVoice ends whatever the disconnected browser endpoint was
// part of: a queued start, or the running session that uses it.
func (s *Server) releaseWebRealtimeVoice(web *webRealtimeAudio) {
	c := &s.realtimeVoice
	c.mu.Lock()
	if c.pending == web {
		c.pending = nil
	}
	if active := c.active; active != nil && active.audio == web {
		active.requestStop()
	}
	c.mu.Unlock()
}

func (s *Server) stopRealtimeVoice() bool {
	c := &s.realtimeVoice
	c.mu.Lock()
	active := c.active
	pending := c.pending
	c.pending = nil
	// Mark the session stopped while holding the lock, so a rotation that
	// ends it concurrently sees the stop and does not requeue its endpoint.
	if active != nil {
		active.requestStop()
	}
	c.mu.Unlock()
	if pending != nil {
		pending.Close("")
	}
	return active != nil || pending != nil
}

func (s *Server) setRealtimeVoiceMuted(microphone, speaker *bool) bool {
	c := &s.realtimeVoice
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active == nil {
		return false
	}
	if microphone != nil {
		active.muted.Store(*microphone)
	}
	if speaker != nil {
		active.silenced.Store(*speaker)
	}
	return true
}

// handleRealtimeVoice serves GET /api/realtime/voice.
func (s *Server) handleRealtimeVoice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	status := s.RealtimeVoiceStatus()
	status.HTTPSPort = s.HTTPSPort()
	writeJSON(w, status)
}

// handleRealtimeVoiceStop serves POST /api/realtime/voice/stop. It ends the
// session whichever route it uses.
func (s *Server) handleRealtimeVoiceStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.stopRealtimeVoice()
	writeJSON(w, s.RealtimeVoiceStatus())
}

// handleRealtimeVoiceMute serves POST /api/realtime/voice/mute. The body sets
// either or both of {"muted": bool} (microphone) and {"speaker_muted": bool};
// an omitted field keeps its state.
func (s *Server) handleRealtimeVoiceMute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Muted        *bool `json:"muted"`
		SpeakerMuted *bool `json:"speaker_muted"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if body.Muted == nil && body.SpeakerMuted == nil {
		http.Error(w, "Invalid request: set muted or speaker_muted", http.StatusBadRequest)
		return
	}
	if !s.setRealtimeVoiceMuted(body.Muted, body.SpeakerMuted) {
		http.Error(w, "no realtime voice session is active", http.StatusConflict)
		return
	}
	writeJSON(w, s.RealtimeVoiceStatus())
}
