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
	stop      chan struct{}
	stopOnce  sync.Once
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
// A browser start queues its endpoint in pending and wakes the loop. The loop
// cannot tell which source woke it, so a browser start that is released
// before the loop claims it leaves cancelled set: the loop then skips that
// wakeup instead of opening the board microphone for a session nobody asked
// for. Claiming the next session and publishing it happen under one lock, so
// a browser start never slips between them unseen.
type realtimeVoiceController struct {
	mu        sync.Mutex
	activate  func()
	pending   *webRealtimeAudio
	cancelled bool
	active    *RealtimeVoiceSession
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
// to open and publishes it as connecting: a browser waiting to start, or
// otherwise the device. It returns nil when the wakeup came from a browser
// start that has since been released, so no session should open. The caller
// must pass a non-nil result to EndRealtimeVoiceSession.
func (s *Server) ClaimRealtimeVoiceSession(cfg Config) *RealtimeVoiceSession {
	if s == nil {
		audio := NewDeviceRealtimeAudioIO(cfg)
		return &RealtimeVoiceSession{route: audio.Route(), audio: audio, startedAt: time.Now(), stop: make(chan struct{})}
	}
	c := &s.realtimeVoice
	c.mu.Lock()
	defer c.mu.Unlock()
	var audio RealtimeAudioIO
	switch {
	case c.pending != nil && !c.pending.closed():
		audio = c.pending
	case c.cancelled:
		c.cancelled = false
		c.pending = nil
		return nil
	default:
		audio = NewDeviceRealtimeAudioIO(cfg)
	}
	c.pending = nil
	c.cancelled = false
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
	return c.pending != nil
}

// Audio returns the endpoint the session captures and plays through.
func (v *RealtimeVoiceSession) Audio() RealtimeAudioIO {
	if v == nil {
		return nil
	}
	return v.audio
}

// RequeueRealtimeAudioIO keeps a browser endpoint for the next session when
// the provider rotates the current one. It reports whether the endpoint was
// kept; the caller closes it otherwise.
func (s *Server) RequeueRealtimeAudioIO(audio RealtimeAudioIO) bool {
	web, ok := audio.(*webRealtimeAudio)
	if s == nil || !ok || web.closed() {
		return false
	}
	c := &s.realtimeVoice
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending != nil || c.activate == nil {
		return false
	}
	c.pending = web
	c.cancelled = false
	return true
}

// EndRealtimeVoiceSession withdraws session from the published status.
func (s *Server) EndRealtimeVoiceSession(session *RealtimeVoiceSession) {
	if s == nil || session == nil {
		return
	}
	c := &s.realtimeVoice
	c.mu.Lock()
	if c.active == session {
		c.active = nil
	}
	c.mu.Unlock()
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
	c.cancelled = false
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
		c.cancelled = true
	}
	active := c.active
	c.mu.Unlock()
	if active != nil && active.audio == web {
		active.requestStop()
	}
}

func (s *Server) stopRealtimeVoice() bool {
	c := &s.realtimeVoice
	c.mu.Lock()
	active := c.active
	pending := c.pending
	if pending != nil {
		c.pending = nil
		c.cancelled = true
	}
	c.mu.Unlock()
	if pending != nil {
		pending.Close("")
	}
	if active != nil {
		active.requestStop()
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
