package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"

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
	// RealtimeAudioRouteText opens no microphone or speaker. A session that
	// only a typed /api/chat message started uses it: replies are text, and
	// the Web UI does not show it as a voice conversation.
	RealtimeAudioRouteText RealtimeAudioRoute = "text"
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

// textRealtimeAudio is the endpoint of a text session. It has no recording or
// playback backend; the session loop must not open audio for this route.
type textRealtimeAudio struct{}

func (textRealtimeAudio) Route() RealtimeAudioRoute         { return RealtimeAudioRouteText }
func (textRealtimeAudio) Recording() AudioRecordingBackend  { return nil }
func (textRealtimeAudio) Playback() tts.AudioServiceBackend { return nil }
func (textRealtimeAudio) Close(string)                      {}

// RealtimeVoiceSession is the control surface of one realtime voice
// conversation. The daemon owns the session loop; the Web UI reads its status
// and asks it to stop or to mute either side through the Server.
//
// A conversation outlives the provider sessions that carry it: when the
// provider caps a session's lifetime, the loop reconnects with the same
// RealtimeVoiceSession, so its audio endpoint, mute state and stop request
// carry over. A nil *RealtimeVoiceSession is a device conversation without
// Web UI controls, as the loop runs without an agent server.
type RealtimeVoiceSession struct {
	audio    RealtimeAudioIO
	ready    atomic.Bool
	muted    atomic.Bool
	silenced atomic.Bool
	// muteChanged wakes the session loop after a mute change, so it can act
	// on audio that is already queued.
	muteChanged chan struct{}
	stop        chan struct{}
	stopOnce    sync.Once
}

// NewRealtimeVoiceSession returns a conversation on audio that no server
// publishes, for a loop that runs without the Web UI controller.
func NewRealtimeVoiceSession(audio RealtimeAudioIO) *RealtimeVoiceSession {
	return newRealtimeVoiceSession(audio)
}

func newRealtimeVoiceSession(audio RealtimeAudioIO) *RealtimeVoiceSession {
	return &RealtimeVoiceSession{audio: audio, muteChanged: make(chan struct{}, 1), stop: make(chan struct{})}
}

// Route reports where this session captures and plays audio.
func (v *RealtimeVoiceSession) Route() RealtimeAudioRoute {
	if v == nil {
		return RealtimeAudioRouteDevice
	}
	return v.audio.Route()
}

// Audio returns the endpoint the session captures and plays through, or nil
// for a nil session.
func (v *RealtimeVoiceSession) Audio() RealtimeAudioIO {
	if v == nil {
		return nil
	}
	return v.audio
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

// MuteChanged receives after the microphone or speaker mute changes.
func (v *RealtimeVoiceSession) MuteChanged() <-chan struct{} {
	if v == nil {
		return nil
	}
	return v.muteChanged
}

// StopRequested is closed when the conversation is asked to end: the user
// pressed End, or the browser that carries its audio disconnected.
func (v *RealtimeVoiceSession) StopRequested() <-chan struct{} {
	if v == nil {
		return nil
	}
	return v.stop
}

// Stopped reports whether the conversation was asked to end.
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

// RequestStop asks the conversation to end. It is safe to call more than
// once and after the conversation has ended.
func (v *RealtimeVoiceSession) RequestStop() {
	if v != nil {
		v.stopOnce.Do(func() { close(v.stop) })
	}
}

// voice reports whether the session carries audio, as opposed to a text
// session.
func (v *RealtimeVoiceSession) voice() bool {
	return v.Route() != RealtimeAudioRouteText
}

// RealtimeVoiceStatus is the public state of the realtime voice mode.
type RealtimeVoiceStatus struct {
	// Available is true while the agent's voice loop (realtime, or Classic
	// stt in continuous session mode) can open a voice session.
	Available bool `json:"available"`
	// State is idle, connecting or active.
	State string             `json:"state"`
	Route RealtimeAudioRoute `json:"route,omitempty"`
	Muted bool               `json:"muted"`
	// SpeakerMuted is true while reply audio is dropped instead of played.
	SpeakerMuted bool `json:"speaker_muted"`
	// HTTPSPort is the agent's HTTPS port, or 0 when HTTPS is off. A page
	// opened over plain HTTP uses it to reopen itself where the browser
	// allows microphone access.
	HTTPSPort int `json:"https_port,omitempty"`
}

// realtimeVoiceController joins Web UI requests with the realtime session
// loop. Its zero value is ready for use.
//
// A browser start queues its endpoint in pending and wakes the loop; device
// activations (wake word, MCU, task updates) and chat requests are tracked by
// the loop itself. The loop asks for a browser endpoint first and otherwise
// opens the route its activation asked for, so a browser start withdrawn
// before the loop reaches it opens nothing. Claiming the next session and
// publishing it happen under one lock, so a browser start never slips between
// them.
//
// A session that was asked to stop is over for every observer, even while
// the loop is still tearing it down: the status reports idle and a new
// browser start may queue behind it. A text session is never shown as voice
// mode, and a browser start ends it so the voice conversation can begin.
type realtimeVoiceController struct {
	mu       sync.Mutex
	activate func()
	pending  *webRealtimeAudio
	active   *RealtimeVoiceSession
}

var (
	errRealtimeVoiceUnavailable = errors.New("voice is not available; set the input mode to realtime or stt")
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

// ClaimRealtimeVoiceSession selects the audio endpoint for the conversation
// about to open and publishes it as connecting. A browser waiting to start
// wins; otherwise requested names the route the loop's activation asked for:
// RealtimeAudioRouteDevice for a wake word, button press or task update,
// RealtimeAudioRouteText for a typed chat request, or "" for neither. It
// returns nil when there is nothing to open, for example when the only
// wakeup came from a browser start that was since withdrawn. The caller must
// pass a non-nil result to EndRealtimeVoiceSession once the conversation is
// over.
func (s *Server) ClaimRealtimeVoiceSession(cfg Config, requested RealtimeAudioRoute) *RealtimeVoiceSession {
	c := &s.realtimeVoice
	c.mu.Lock()
	defer c.mu.Unlock()
	var audio RealtimeAudioIO
	switch {
	case c.pending != nil && !c.pending.closed():
		// A closed endpoint is not replaced by the device: the browser asked
		// for its own audio.
		audio = c.pending
	case requested == RealtimeAudioRouteDevice:
		audio = NewDeviceRealtimeAudioIO(cfg)
	case requested == RealtimeAudioRouteText:
		audio = textRealtimeAudio{}
	}
	c.pending = nil
	if audio == nil {
		return nil
	}
	c.active = newRealtimeVoiceSession(audio)
	return c.active
}

// RealtimeVoicePending reports whether a browser voice start is waiting for
// the session loop.
func (s *Server) RealtimeVoicePending() bool {
	c := &s.realtimeVoice
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending != nil && !c.pending.closed()
}

// EndRealtimeVoiceSession withdraws session from the published status and
// releases its audio endpoint. reason is shown to the browser that carried
// the audio; it is empty for a normal end.
func (s *Server) EndRealtimeVoiceSession(session *RealtimeVoiceSession, reason string) {
	c := &s.realtimeVoice
	c.mu.Lock()
	if c.active == session {
		c.active = nil
	}
	c.mu.Unlock()
	session.audio.Close(reason)
}

// RealtimeVoiceStatus reports the current realtime voice state.
func (s *Server) RealtimeVoiceStatus() RealtimeVoiceStatus {
	c := &s.realtimeVoice
	c.mu.Lock()
	defer c.mu.Unlock()
	status := RealtimeVoiceStatus{Available: c.activate != nil, State: "idle"}
	switch active := c.runningVoice(); {
	case active != nil:
		status.State = "connecting"
		if active.ready.Load() {
			status.State = "active"
		}
		status.Route = active.Route()
		status.Muted = active.muted.Load()
		status.SpeakerMuted = active.silenced.Load()
	case c.pending != nil:
		status.State = "connecting"
		status.Route = RealtimeAudioRouteWeb
	}
	return status
}

// running returns the published session unless it was asked to stop. The
// caller holds c.mu.
func (c *realtimeVoiceController) running() *RealtimeVoiceSession {
	if c.active == nil || c.active.Stopped() {
		return nil
	}
	return c.active
}

// runningWebVoice returns the running conversation when a browser carries
// its audio, or nil.
func (s *Server) runningWebVoice() *RealtimeVoiceSession {
	c := &s.realtimeVoice
	c.mu.Lock()
	defer c.mu.Unlock()
	if active := c.runningVoice(); active != nil && active.Route() == RealtimeAudioRouteWeb {
		return active
	}
	return nil
}

// runningVoice is running without a text session. The caller holds c.mu.
func (c *realtimeVoiceController) runningVoice() *RealtimeVoiceSession {
	if active := c.running(); active != nil && active.voice() {
		return active
	}
	return nil
}

// startWebRealtimeVoice queues a browser endpoint and wakes the session loop.
// A running text session is ended so the loop can start the voice session.
func (s *Server) startWebRealtimeVoice(web *webRealtimeAudio) error {
	c := &s.realtimeVoice
	c.mu.Lock()
	if c.activate == nil {
		c.mu.Unlock()
		return errRealtimeVoiceUnavailable
	}
	if c.runningVoice() != nil || c.pending != nil {
		c.mu.Unlock()
		return errRealtimeVoiceBusy
	}
	if text := c.running(); text != nil {
		text.RequestStop()
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
		active.RequestStop()
	}
	c.mu.Unlock()
}

// stopRealtimeVoice ends the running voice session and withdraws a queued
// browser start. A text session is not voice mode and is left running.
func (s *Server) stopRealtimeVoice() {
	c := &s.realtimeVoice
	c.mu.Lock()
	pending := c.pending
	c.pending = nil
	if c.active != nil && c.active.voice() {
		c.active.RequestStop()
	}
	c.mu.Unlock()
	if pending != nil {
		pending.Close("")
	}
}

func (s *Server) setRealtimeVoiceMuted(microphone, speaker *bool) bool {
	c := &s.realtimeVoice
	c.mu.Lock()
	active := c.runningVoice()
	c.mu.Unlock()
	if active == nil {
		return false
	}
	active.SetMuted(microphone, speaker)
	return true
}

// SetMuted changes the microphone and/or speaker mute; a nil argument keeps
// that side's state. The session loop is woken through MuteChanged.
func (v *RealtimeVoiceSession) SetMuted(microphone, speaker *bool) {
	if v == nil {
		return
	}
	if microphone != nil {
		v.muted.Store(*microphone)
	}
	if speaker != nil {
		v.silenced.Store(*speaker)
	}
	select {
	case v.muteChanged <- struct{}{}:
	default:
	}
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
