package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const (
	audioStartPlaybackRetryDelay    = 200 * time.Millisecond
	audioStartPlaybackRetryAttempts = 12
	audioStartPlaybackBusyTimeout   = 5 * time.Second
)

// AudioFormat specifies audio stream parameters
type AudioFormat struct {
	SampleRate uint32 `json:"sample_rate"`
	Channels   uint32 `json:"channels"`
	BitWidth   uint32 `json:"bit_width"`
}

// RecordStartResult contains the session ID for a recording session
type RecordStartResult struct {
	SessionID uint64 `json:"session_id"`
}

// AudioChunkResult contains PCM data from a recording session
type AudioChunkResult struct {
	PCM         []byte `json:"pcm"`
	EndOfStream bool   `json:"end_of_stream"`
}

// PlaybackStartResult contains the session ID for a playback session
type PlaybackStartResult struct {
	SessionID uint64 `json:"session_id"`
}

type audioStatusError struct {
	op     string
	status string
}

func (e *audioStatusError) Error() string {
	return fmt.Sprintf("%s failed: %s", e.op, e.status)
}

func isRetryableAudioStartPlaybackError(err error) bool {
	if err == nil {
		return false
	}
	var statusErr *audioStatusError
	return errors.As(err, &statusErr) && statusErr.op == "start_playback" && statusErr.status == "SERVICE_RECOVERING"
}

func (c *AudioServiceClient) startPlaybackOnce(format AudioFormat) (*PlaybackStartResult, error) {
	req := audioRequest{
		Op:         "start_playback",
		SampleRate: format.SampleRate,
		Channels:   format.Channels,
		BitWidth:   format.BitWidth,
	}

	resp, _, err := c.sendRequest(req, 5*time.Second)
	if err != nil {
		return nil, err
	}

	if resp.Status != "OK" {
		return nil, &audioStatusError{op: "start_playback", status: resp.Status}
	}

	return &PlaybackStartResult{SessionID: uint64(resp.SessionID)}, nil
}

// AudioHealthResult contains audio service health status
type AudioHealthResult struct {
	RecordingActive  bool   `json:"recording_active"`
	PlaybackActive   bool   `json:"playback_active"`
	RecordSessions   uint32 `json:"record_sessions"`
	PlaybackSessions uint32 `json:"playback_sessions"`
}

// AudioServiceClient is a client for the audio_service Unix socket
type AudioServiceClient struct {
	socketPath string
}

// NewAudioServiceClient creates a new audio service client
func NewAudioServiceClient(socketPath string) *AudioServiceClient {
	return &AudioServiceClient{socketPath: socketPath}
}

// AudioRecordChunkReader reuses one Unix socket for a recording session's
// long-poll chunk reads.
type AudioRecordChunkReader struct {
	conn      net.Conn
	sessionID uint64
}

func (c *AudioServiceClient) OpenRecordChunkReader(sessionID uint64) (*AudioRecordChunkReader, error) {
	conn, err := net.DialTimeout("unix", c.socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	return &AudioRecordChunkReader{conn: conn, sessionID: sessionID}, nil
}

func (r *AudioRecordChunkReader) Read(timeoutMs uint32) (*AudioChunkResult, error) {
	return r.readUntil(timeoutMs, time.Time{})
}

func (r *AudioRecordChunkReader) readUntil(timeoutMs uint32, deadline time.Time) (*AudioChunkResult, error) {
	if r == nil || r.conn == nil {
		return nil, fmt.Errorf("record chunk reader is closed")
	}
	req := audioRequest{Op: "read_record_chunk", SessionID: r.sessionID, TimeoutMs: timeoutMs}
	timeout := time.Duration(timeoutMs)*time.Millisecond + 5*time.Second
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("record chunk read deadline exceeded")
		}
		if timeout > remaining {
			timeout = remaining
		}
	}
	_ = r.conn.SetDeadline(time.Now().Add(timeout))
	headerJSON, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	if err := writeUdsMessage(r.conn, udsMessage{HeaderJSON: string(headerJSON)}); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	respMsg, err := readUdsMessage(r.conn)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var resp audioResponse
	if err := json.Unmarshal([]byte(respMsg.HeaderJSON), &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if resp.Status == "TIMEOUT" {
		return &AudioChunkResult{}, nil
	}
	if resp.Status != "OK" {
		return nil, fmt.Errorf("read_record_chunk failed: %s", resp.Status)
	}
	return &AudioChunkResult{PCM: respMsg.Payload, EndOfStream: resp.EndOfStream}, nil
}

func (r *AudioRecordChunkReader) Close() error {
	if r == nil || r.conn == nil {
		return nil
	}
	err := r.conn.Close()
	r.conn = nil
	return err
}

func isTransientAudioServiceError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"connection refused",
		"connection reset by peer",
		"broken pipe",
		"no such file or directory",
		"temporary failure",
		"temporarily unavailable",
		"i/o timeout",
		"eof",
		"use of closed network connection",
	} {
		if strings.Contains(msg, fragment) {
			return true
		}
	}
	return false
}

// audioRequest is the wire format for requests
type audioRequest struct {
	Op         string `json:"op"`
	SampleRate uint32 `json:"sample_rate,omitempty"`
	Channels   uint32 `json:"channels,omitempty"`
	BitWidth   uint32 `json:"bit_width,omitempty"`
	Volume     uint32 `json:"volume,omitempty"`
	SessionID  uint64 `json:"session_id,omitempty"`
	TimeoutMs  uint32 `json:"timeout_ms,omitempty"`
	IsFinal    bool   `json:"is_final,omitempty"`
	payload    []byte // sent as binary payload, not in JSON
}

// audioResponse is the wire format for responses
type audioResponse struct {
	Status           string       `json:"status"`
	SessionID        stringUint64 `json:"session_id,omitempty"`
	EndOfStream      bool         `json:"end_of_stream,omitempty"`
	RecordingActive  bool         `json:"recording_active,omitempty"`
	PlaybackActive   bool         `json:"playback_active,omitempty"`
	RecordSessions   uint32       `json:"record_sessions,omitempty"`
	PlaybackSessions uint32       `json:"playback_sessions,omitempty"`
	Volume           uint32       `json:"volume,omitempty"`
}

// stringUint64 handles JSON values that may be either a string or number
type stringUint64 uint64

func (s *stringUint64) UnmarshalJSON(data []byte) error {
	// Try as number first
	var n uint64
	if err := json.Unmarshal(data, &n); err == nil {
		*s = stringUint64(n)
		return nil
	}
	// Try as string
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	var parsed uint64
	_, err := fmt.Sscanf(str, "%d", &parsed)
	if err != nil {
		return fmt.Errorf("parse session_id %q: %w", str, err)
	}
	*s = stringUint64(parsed)
	return nil
}

// udsMessage represents the wire format: [4B header_len LE][8B payload_len LE][header JSON][payload]
type udsMessage struct {
	HeaderJSON string
	Payload    []byte
}

// writeUdsMessage writes a message in the wire protocol format
func writeUdsMessage(conn net.Conn, msg udsMessage) error {
	var prefix [12]byte
	binary.LittleEndian.PutUint32(prefix[0:4], uint32(len(msg.HeaderJSON)))
	binary.LittleEndian.PutUint64(prefix[4:12], uint64(len(msg.Payload)))

	if _, err := conn.Write(prefix[:]); err != nil {
		return fmt.Errorf("write prefix: %w", err)
	}
	if len(msg.HeaderJSON) > 0 {
		if _, err := io.WriteString(conn, msg.HeaderJSON); err != nil {
			return fmt.Errorf("write header: %w", err)
		}
	}
	if len(msg.Payload) > 0 {
		if _, err := conn.Write(msg.Payload); err != nil {
			return fmt.Errorf("write payload: %w", err)
		}
	}
	return nil
}

// readUdsMessage reads a message in the wire protocol format
func readUdsMessage(conn net.Conn) (*udsMessage, error) {
	var prefix [12]byte
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		return nil, fmt.Errorf("read prefix: %w", err)
	}

	headerLen := binary.LittleEndian.Uint32(prefix[0:4])
	payloadLen := binary.LittleEndian.Uint64(prefix[4:12])

	msg := &udsMessage{}

	// Read header JSON
	if headerLen > 0 {
		headerBuf := make([]byte, headerLen)
		if _, err := io.ReadFull(conn, headerBuf); err != nil {
			return nil, fmt.Errorf("read header: %w", err)
		}
		msg.HeaderJSON = string(headerBuf)
	}

	// Read payload
	if payloadLen > 0 {
		msg.Payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(conn, msg.Payload); err != nil {
			return nil, fmt.Errorf("read payload: %w", err)
		}
	}

	return msg, nil
}

// sendRequest sends a request and receives a response using the UDS wire protocol
func (c *AudioServiceClient) sendRequest(req audioRequest, timeout time.Duration) (*audioResponse, []byte, error) {
	conn, err := net.DialTimeout("unix", c.socketPath, 5*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	if timeout > 0 {
		conn.SetDeadline(time.Now().Add(timeout))
	}

	// Marshal request header JSON
	headerJSON, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}

	// Send request (header JSON + optional binary payload)
	if err := writeUdsMessage(conn, udsMessage{HeaderJSON: string(headerJSON), Payload: req.payload}); err != nil {
		return nil, nil, fmt.Errorf("write request: %w", err)
	}

	// Read response
	respMsg, err := readUdsMessage(conn)
	if err != nil {
		return nil, nil, fmt.Errorf("read response: %w", err)
	}

	var resp audioResponse
	if err := json.Unmarshal([]byte(respMsg.HeaderJSON), &resp); err != nil {
		return nil, nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return &resp, respMsg.Payload, nil
}

// StartRecording starts a new recording session
func (c *AudioServiceClient) StartRecording(format AudioFormat) (*RecordStartResult, error) {
	req := audioRequest{
		Op:         "start_recording",
		SampleRate: format.SampleRate,
		Channels:   format.Channels,
		BitWidth:   format.BitWidth,
	}

	resp, _, err := c.sendRequest(req, 5*time.Second)
	if err != nil {
		return nil, err
	}

	if resp.Status != "OK" {
		return nil, fmt.Errorf("start_recording failed: %s", resp.Status)
	}

	return &RecordStartResult{SessionID: uint64(resp.SessionID)}, nil
}

// ReadRecordChunk reads a PCM chunk from a recording session (long-poll)
func (c *AudioServiceClient) ReadRecordChunk(sessionID uint64, timeoutMs uint32) (*AudioChunkResult, error) {
	return c.readRecordChunkUntil(sessionID, timeoutMs, time.Time{})
}

func (c *AudioServiceClient) readRecordChunkUntil(sessionID uint64, timeoutMs uint32, deadline time.Time) (*AudioChunkResult, error) {
	req := audioRequest{Op: "read_record_chunk", SessionID: sessionID, TimeoutMs: timeoutMs}
	timeout := time.Duration(timeoutMs+5000) * time.Millisecond
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("record chunk read deadline exceeded")
		}
		if timeout > remaining {
			timeout = remaining
		}
	}
	resp, payload, err := c.sendRequest(req, timeout)
	if err != nil {
		return nil, err
	}
	if resp.Status == "TIMEOUT" {
		return &AudioChunkResult{}, nil
	}
	if resp.Status != "OK" {
		return nil, fmt.Errorf("read_record_chunk failed: %s", resp.Status)
	}
	return &AudioChunkResult{PCM: payload, EndOfStream: resp.EndOfStream}, nil
}

// StopRecording stops a recording session
func (c *AudioServiceClient) StopRecording(sessionID uint64) error {
	req := audioRequest{
		Op:        "stop_recording",
		SessionID: sessionID,
	}

	resp, _, err := c.sendRequest(req, 5*time.Second)
	if err != nil {
		return err
	}

	if resp.Status != "OK" {
		return fmt.Errorf("stop_recording failed: %s", resp.Status)
	}

	return nil
}

// StartPlayback starts a new playback session
func (c *AudioServiceClient) StartPlayback(format AudioFormat) (*PlaybackStartResult, error) {
	var lastErr error
	for attempt := 0; attempt < audioStartPlaybackRetryAttempts; attempt++ {
		playback, err := c.startPlaybackOnce(format)
		if err == nil {
			return playback, nil
		}
		lastErr = err
		if !isRetryableAudioStartPlaybackError(lastErr) || attempt == audioStartPlaybackRetryAttempts-1 {
			break
		}
		if err := waitForPlaybackDrain(context.Background(), c, audioStartPlaybackBusyTimeout); err != nil {
			time.Sleep(audioStartPlaybackRetryDelay)
		}
	}

	return nil, lastErr
}

// WritePlayChunk writes a PCM chunk to a playback session
func (c *AudioServiceClient) WritePlayChunk(sessionID uint64, data []byte, isFinal bool) error {
	req := audioRequest{
		Op:        "write_play_chunk",
		SessionID: sessionID,
		IsFinal:   isFinal,
		payload:   data,
	}

	resp, _, err := c.sendRequest(req, 5*time.Second)
	if err != nil {
		return err
	}

	if resp.Status != "OK" {
		return fmt.Errorf("write_play_chunk failed: %s", resp.Status)
	}

	return nil
}

// StopPlayback stops a playback session
func (c *AudioServiceClient) StopPlayback(sessionID uint64) error {
	req := audioRequest{
		Op:        "stop_playback",
		SessionID: sessionID,
	}

	resp, _, err := c.sendRequest(req, 5*time.Second)
	if err != nil {
		return err
	}

	if resp.Status != "OK" {
		return fmt.Errorf("stop_playback failed: %s", resp.Status)
	}

	return nil
}

// SetPlaybackVolume sets the default playback volume and applies it to active sessions.
func (c *AudioServiceClient) SetPlaybackVolume(volume int) error {
	if volume < 0 || volume > 100 {
		return fmt.Errorf("volume must be in range 0..100, got %d", volume)
	}

	req := audioRequest{
		Op:     "set_playback_volume",
		Volume: uint32(volume),
	}

	resp, _, err := c.sendRequest(req, 5*time.Second)
	if err != nil {
		return err
	}

	if resp.Status != "OK" {
		return fmt.Errorf("set_playback_volume failed: %s", resp.Status)
	}

	return nil
}

// GetPlaybackVolume returns the current playback volume in the range 0..100.
func (c *AudioServiceClient) GetPlaybackVolume() (int, error) {
	req := audioRequest{
		Op: "get_playback_volume",
	}

	resp, _, err := c.sendRequest(req, 5*time.Second)
	if err != nil {
		return 0, err
	}

	if resp.Status != "OK" {
		return 0, fmt.Errorf("get_playback_volume failed: %s", resp.Status)
	}

	return int(resp.Volume), nil
}

// Health checks the audio service health
func (c *AudioServiceClient) Health() (*AudioHealthResult, error) {
	req := audioRequest{
		Op: "health",
	}

	resp, _, err := c.sendRequest(req, 5*time.Second)
	if err != nil {
		return nil, err
	}

	if resp.Status != "OK" {
		return nil, fmt.Errorf("health check failed: %s", resp.Status)
	}

	return &AudioHealthResult{
		RecordingActive:  resp.RecordingActive,
		PlaybackActive:   resp.PlaybackActive,
		RecordSessions:   resp.RecordSessions,
		PlaybackSessions: resp.PlaybackSessions,
	}, nil
}
