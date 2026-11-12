// Realtime voice mode.
//
// The agent runs one realtime voice session at a time. A session started on
// the board (wake word, MCU button) captures and plays audio on the device; a
// session started here captures and plays audio in this browser over
// /api/realtime/voice/ws. The page shows the same voice mode for both and
// only owns the media of a session it started itself.
const voiceStageEl = document.getElementById('voiceStage');
const voiceOrbEl = document.getElementById('voiceOrb');
const voiceStatusEl = document.getElementById('voiceStatus');
const voiceMuteBtn = document.getElementById('voiceMuteBtn');
const voiceSpeakerBtn = document.getElementById('voiceSpeakerBtn');
const voiceStatusPollMs = 1500;
const voiceStartTimeoutMs = 30000;
// How long to wait for a suspended audio context to start. A browser that
// does not allow audio leaves resume() pending rather than rejecting it.
const voiceResumeTimeoutMs = 2000;
const voiceCaptureFrameMs = 20;
const voicePlaybackLeadSeconds = 0.04;
// Keep at most this much microphone audio waiting to be sent. On a slow link
// newer speech matters more than a backlog, which would reach the agent long
// after it was spoken.
const voiceSendBacklogBytes = 32000;
// A status poll may fail this many times in a row before the page gives up
// on the session it shows; one dropped request must not hide a conversation.
const voiceStatusFailureLimit = 3;
const voiceIdleStatus = {available: false, state: 'idle', muted: false, speaker_muted: false};
const defaultInputPlaceholder = inputEl.placeholder;

// voiceStatus is the agent's view, refreshed by polling. voiceLocal is the
// browser session this tab started, if any; it owns the microphone, the
// audio context and the socket.
let voiceStatus = voiceIdleStatus;
let voiceActivity = 'idle';
let voiceLocal = null;
let voiceStatusRequest = 0;
let voiceStatusFailures = 0;
// True from pressing End until the agent has answered. A stop names no
// session, so a session started meanwhile could be the one it ends.
let voiceStopping = false;

// createVoiceResampler converts Float32 frames between sample rates with
// linear interpolation, carrying the fractional position across frames.
function createVoiceResampler(fromRate, toRate) {
    const step = fromRate / toRate;
    let position = 0;
    let previous = 0;
    return function(input) {
        const output = [];
        // Index 0 is the last sample of the previous frame.
        const length = input.length + 1;
        const sampleAt = i => (i === 0 ? previous : input[i - 1]);
        while (position + 1 < length) {
            const index = Math.floor(position);
            const fraction = position - index;
            output.push(sampleAt(index) + (sampleAt(index + 1) - sampleAt(index)) * fraction);
            position += step;
        }
        position -= length - 1;
        if (input.length) previous = input[input.length - 1];
        return output;
    };
}

function floatToPCM16(samples) {
    const pcm = new Int16Array(samples.length);
    for (let i = 0; i < samples.length; i++) {
        const value = Math.max(-1, Math.min(1, samples[i]));
        pcm[i] = value < 0 ? value * 0x8000 : value * 0x7fff;
    }
    return pcm;
}

function pcm16ToChannels(buffer, channels) {
    const bytes = buffer.byteLength - (buffer.byteLength % (2 * channels));
    const pcm = new Int16Array(buffer, 0, bytes / 2);
    const frames = pcm.length / channels;
    const result = [];
    for (let c = 0; c < channels; c++) {
        const channel = new Float32Array(frames);
        for (let i = 0; i < frames; i++) channel[i] = pcm[i * channels + c] / 0x8000;
        result.push(channel);
    }
    return result;
}

// patchVoiceStatus applies a change this page just made, ahead of the agent
// confirming it. Polls already in flight predate the change and are dropped.
function patchVoiceStatus(patch) {
    voiceStatusRequest++;
    voiceStatus = Object.assign({}, voiceStatus, patch);
}

function voiceModeActive() {
    return !!voiceLocal || voiceStatus.state !== 'idle';
}

// voiceView is everything the page shows, derived in one place so the orb,
// the label and the buttons cannot disagree.
function voiceView() {
    const local = voiceLocal;
    const playing = !!local && local.context && local.nextPlayTime > local.context.currentTime;
    return {
        active: voiceModeActive(),
        connecting: local ? !local.capturing : voiceStatus.state === 'connecting',
        // A browser session mutes in this tab at once; the agent's state
        // follows, and is all there is for a session elsewhere.
        muted: local ? local.muted : !!voiceStatus.muted,
        speakerMuted: local ? local.speakerMuted : !!voiceStatus.speaker_muted,
        // The reply is generated faster than it is spoken, so a browser
        // session keeps "speaking" until its queued audio has played.
        activity: playing && voiceActivity === 'idle' ? 'speaking' : voiceActivity,
        where: local ? '' : voiceStatus.route === 'web' ? ' · audio in another browser' : ' · audio on device'
    };
}

function voiceStatusLabel(view) {
    if (view.connecting) return 'Connecting…';
    if (view.muted && view.speakerMuted) return 'Microphone and speaker muted';
    if (view.muted) return 'Microphone muted';
    if (view.speakerMuted && view.activity === 'speaking') return 'Speaker muted';
    const label = {listening: 'Listening', thinking: 'Thinking', speaking: 'Speaking'}[view.activity] || 'Ready';
    return label + view.where;
}

// The orb floats just above whatever sits under the message list: the
// activity row, a pending steer and the composer, which grows with its input
// and attachments. Follow the list's bottom edge instead of a fixed offset.
new ResizeObserver(function() {
    const content = voiceStageEl.parentElement;
    content.style.setProperty('--voice-stage-bottom', (content.clientHeight - conversationEl.offsetTop - conversationEl.offsetHeight) + 'px');
}).observe(conversationEl);

function renderRealtimeVoice() {
    const view = voiceView();
    const wasActive = document.body.classList.contains('voice-mode');
    voiceStageEl.classList.toggle('hidden', !view.active);
    document.body.classList.toggle('voice-mode', view.active);
    renderVoiceToggle(voiceMuteBtn, view.active, view.muted, 'microphone');
    renderVoiceToggle(voiceSpeakerBtn, view.active, view.speakerMuted, 'speaker');
    renderComposerPrimary();
    voiceOrbEl.dataset.activity = view.connecting ? 'connecting' : view.muted && view.activity === 'listening' ? 'idle' : view.activity;
    voiceStatusEl.textContent = voiceStatusLabel(view);
    inputEl.placeholder = view.active ? 'Type a message…' : defaultInputPlaceholder;
    if (!view.active) voiceOrbEl.style.removeProperty('--voice-level');
    // Entering voice mode adds room under the last message for the orb; keep
    // the reader at the bottom so the latest message stays above the fade.
    if (view.active && !wasActive) scrollToBottom();
}

function renderVoiceToggle(button, visible, off, name) {
    button.classList.toggle('hidden', !visible);
    button.classList.toggle('off', off);
    button.setAttribute('aria-pressed', off ? 'true' : 'false');
    button.title = (off ? 'Unmute ' : 'Mute ') + name;
    button.setAttribute('aria-label', button.title);
}

// refreshRealtimeVoiceStatus reads the agent's state. Requests may overlap;
// only the newest one is applied, so a refresh made right after an action is
// never lost behind an older poll.
async function refreshRealtimeVoiceStatus() {
    // While a stop is on its way the agent may still report the session as
    // running. Showing that would offer End again; wait for the stop to settle.
    if (voiceStopping) return;
    const request = ++voiceStatusRequest;
    let next = null;
    try {
        const res = await fetch('/api/realtime/voice', {cache: 'no-store'});
        if (res.ok) next = await res.json();
    } catch (err) {
        // Counted below.
    }
    if (request !== voiceStatusRequest) return;
    if (next) {
        voiceStatusFailures = 0;
        if (voiceStatus.state === 'idle' || next.state === 'idle') voiceActivity = 'idle';
        voiceStatus = Object.assign({}, voiceIdleStatus, next);
    } else if (++voiceStatusFailures >= voiceStatusFailureLimit) {
        // The agent is unreachable, or too old to have a voice endpoint.
        voiceStatus = voiceIdleStatus;
        voiceActivity = 'idle';
    }
    renderRealtimeVoice();
}

function startRealtimeVoiceStatusPolling() {
    refreshRealtimeVoiceStatus().then(startRealtimeVoiceFromLocation);
    setInterval(function() {
        if (!document.hidden || voiceLocal) refreshRealtimeVoiceStatus();
    }, voiceStatusPollMs);
}

// Semantic events give the orb its activity for either audio route.
function handleRealtimeVoiceEvent(event) {
    if (!event || typeof event.kind !== 'string') return;
    let activity = '';
    switch (event.kind) {
        case 'session.ready':
        case 'session.closed':
            voiceActivity = 'idle';
            refreshRealtimeVoiceStatus();
            return;
        case 'input.speech_started': activity = 'listening'; break;
        case 'input.speech_stopped':
        case 'turn.committed': activity = 'thinking'; break;
        case 'response.started':
        case 'text.delta': activity = 'speaking'; break;
        case 'response.terminal':
        case 'stream.discontinuity': activity = 'idle'; break;
        default: return;
    }
    if (!voiceModeActive()) return;
    voiceActivity = activity;
    renderRealtimeVoice();
}

async function startRealtimeVoice() {
    if (voiceModeActive() || voiceStopping) return;
    if (!window.isSecureContext || !navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
        const secureURL = realtimeVoiceSecureURL();
        if (secureURL) {
            // Browsers only expose the microphone to secure pages. Reopen this
            // page over the agent's HTTPS port and start voice there.
            location.assign(secureURL);
            return;
        }
        setComposerHint('Browser voice needs a secure page (HTTPS or localhost) for microphone access', true);
        return;
    }
    const local = {
        socket: null, stream: null, context: null, capture: null,
        // Set by capture_start and playback_start before any audio flows.
        captureRate: 0, resample: null, playbackFormat: null,
        capturing: false, muted: false, speakerMuted: false,
        pending: [], pendingSamples: 0, sendStalled: false,
        sources: new Set(), nextPlayTime: 0, playingTimer: null,
        inputAnalyser: null, outputAnalyser: null, outputGain: null,
        meterFrame: 0, startTimer: null, ended: false, endReason: ''
    };
    voiceLocal = local;
    voiceActivity = 'idle';
    renderRealtimeVoice();
    try {
        // Create the audio context inside the click so the browser lets it
        // run; one created after the permission prompt may stay suspended.
        local.context = new (window.AudioContext || window.webkitAudioContext)({latencyHint: 'interactive'});
        const stream = await navigator.mediaDevices.getUserMedia({
            audio: {channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true}
        });
        if (local.ended) {
            // The user ended voice while the permission prompt was open;
            // release the microphone that was granted afterwards.
            stream.getTracks().forEach(track => track.stop());
            return;
        }
        local.stream = stream;
        applyLocalVoiceMute(local);
        await local.context.audioWorklet.addModule('/web-ui/scripts/voice_capture_worklet.js');
        if (local.ended) return;
        const source = local.context.createMediaStreamSource(local.stream);
        local.inputAnalyser = local.context.createAnalyser();
        local.inputAnalyser.fftSize = 512;
        local.capture = new AudioWorkletNode(local.context, 'aico-voice-capture', {numberOfOutputs: 1, outputChannelCount: [1]});
        local.capture.port.onmessage = event => handleVoiceCaptureFrame(local, event.data);
        source.connect(local.inputAnalyser);
        source.connect(local.capture);
        // The worklet writes no output; connecting it keeps the graph pulling it.
        local.capture.connect(local.context.destination);
        local.outputGain = local.context.createGain();
        local.outputAnalyser = local.context.createAnalyser();
        local.outputAnalyser.fftSize = 512;
        local.outputGain.connect(local.outputAnalyser);
        local.outputAnalyser.connect(local.context.destination);
        applyLocalVoiceMute(local);
        openVoiceSocket(local);
        startVoiceMeter(local);
    } catch (err) {
        teardownLocalVoice(local, voiceStartErrorMessage(err));
    }
}

// realtimeVoiceSecureURL returns this page on the agent's HTTPS port, with a
// marker that offers voice once it loads, or '' when HTTPS is unavailable.
// The marker is a query parameter so it survives a sign-in on the way: the
// HTTPS page is a different site to the browser, which may ask for the device
// password again, and the sign-in redirect keeps the query but not a fragment.
function realtimeVoiceSecureURL() {
    const port = Number(voiceStatus.https_port) || 0;
    if (!port || location.protocol === 'https:') return '';
    const url = new URL(location.href);
    url.protocol = 'https:';
    url.port = String(port);
    url.searchParams.set('voice', '1');
    return url.toString();
}

// A page opened from realtimeVoiceSecureURL asks for one click to start.
// Starting on load would run without a user gesture, and browsers keep the
// audio of such a page suspended.
function startRealtimeVoiceFromLocation() {
    const url = new URL(location.href);
    if (!url.searchParams.has('voice')) return;
    url.searchParams.delete('voice');
    history.replaceState(null, '', url.pathname + url.search + url.hash);
    if (!voiceStatus.available || voiceModeActive()) return;
    setComposerHint('Press the voice button to start talking.', false, true);
    sendBtn.focus();
}

function voiceStartErrorMessage(err) {
    if (err && (err.name === 'NotAllowedError' || err.name === 'SecurityError')) return 'Microphone access was denied';
    if (err && err.name === 'NotFoundError') return 'No microphone was found';
    return 'Could not start voice' + (err && err.message ? ': ' + err.message : '');
}

function openVoiceSocket(local) {
    const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const socket = new WebSocket(scheme + '//' + location.host + '/api/realtime/voice/ws');
    socket.binaryType = 'arraybuffer';
    local.socket = socket;
    local.startTimer = setTimeout(function() {
        if (!local.capturing) teardownLocalVoice(local, 'The voice session did not start in time');
    }, voiceStartTimeoutMs);
    socket.onmessage = function(event) {
        if (typeof event.data === 'string') {
            handleVoiceControlMessage(local, event.data);
        } else {
            playVoicePCM(local, event.data);
        }
    };
    socket.onclose = function(event) {
        // The agent says why it ended a session; a socket that closes without
        // that was cut off, which the user should hear about too.
        teardownLocalVoice(local, local.endReason || (event.wasClean ? '' : 'The voice connection was lost'));
    };
}

function handleVoiceControlMessage(local, text) {
    let message;
    try {
        message = JSON.parse(text);
    } catch (err) {
        return;
    }
    switch (message.type) {
        case 'capture_start':
            local.captureRate = message.format.sample_rate;
            local.resample = createVoiceResampler(local.context.sampleRate, local.captureRate);
            local.pending = [];
            local.pendingSamples = 0;
            beginVoiceCapture(local);
            break;
        case 'capture_stop':
            local.capturing = false;
            break;
        case 'playback_start':
            local.playbackFormat = message.format;
            break;
        case 'playback_stop':
            stopVoicePlayback(local);
            break;
        case 'ended':
            local.endReason = message.reason || '';
            break;
    }
    renderRealtimeVoice();
}

// Capture only counts as started once the audio context runs; a suspended
// context sends and plays nothing while the page would claim to listen.
async function beginVoiceCapture(local) {
    if (local.context.state === 'suspended') {
        let timer = null;
        try {
            await Promise.race([
                local.context.resume(),
                new Promise(resolve => { timer = setTimeout(resolve, voiceResumeTimeoutMs); })
            ]);
        } catch (err) {
            // Checked below.
        } finally {
            clearTimeout(timer);
        }
    }
    if (local.ended) return;
    if (local.context.state !== 'running') {
        teardownLocalVoice(local, 'The browser blocked audio. Press the voice button to start again.');
        return;
    }
    local.capturing = true;
    clearTimeout(local.startTimer);
    // A mute pressed before the agent picked the session up was refused;
    // apply it now so the buttons keep telling the truth.
    if (local.muted || local.speakerMuted) {
        postRealtimeVoiceMute({muted: local.muted, speaker_muted: local.speakerMuted}, 'Could not restore the mute state');
    } else {
        refreshRealtimeVoiceStatus();
    }
    renderRealtimeVoice();
}

function handleVoiceCaptureFrame(local, samples) {
    if (!local.capturing || local.socket.readyState !== WebSocket.OPEN) return;
    const resampled = local.resample(samples);
    if (!resampled.length) return;
    local.pending.push(resampled);
    local.pendingSamples += resampled.length;
    if (local.pendingSamples < local.captureRate * voiceCaptureFrameMs / 1000) return;
    const frame = new Float32Array(local.pendingSamples);
    let offset = 0;
    local.pending.forEach(chunk => {
        frame.set(chunk, offset);
        offset += chunk.length;
    });
    local.pending = [];
    local.pendingSamples = 0;
    if (local.socket.bufferedAmount > voiceSendBacklogBytes) {
        // The connection is not keeping up; drop this frame rather than queue
        // stale audio behind it.
        if (!local.sendStalled) console.warn('[voice] connection is slow; dropping microphone audio');
        local.sendStalled = true;
        return;
    }
    local.sendStalled = false;
    local.socket.send(floatToPCM16(frame).buffer);
}

function playVoicePCM(local, buffer) {
    const format = local.playbackFormat;
    if (!format || buffer.byteLength < 2 * format.channels) return;
    const channels = pcm16ToChannels(buffer, format.channels);
    const audioBuffer = local.context.createBuffer(channels.length, channels[0].length, format.sample_rate);
    channels.forEach((data, index) => audioBuffer.copyToChannel(data, index));
    const source = local.context.createBufferSource();
    source.buffer = audioBuffer;
    source.connect(local.outputGain);
    const now = local.context.currentTime;
    if (local.nextPlayTime < now + voicePlaybackLeadSeconds) local.nextPlayTime = now + voicePlaybackLeadSeconds;
    source.start(local.nextPlayTime);
    local.nextPlayTime += audioBuffer.duration;
    local.sources.add(source);
    source.onended = () => local.sources.delete(source);
    // Show the reply as finished once the queued audio has played out.
    clearTimeout(local.playingTimer);
    local.playingTimer = setTimeout(renderRealtimeVoice, (local.nextPlayTime - now) * 1000 + 50);
}

function stopVoicePlayback(local) {
    local.sources.forEach(source => {
        try {
            source.stop();
        } catch (err) {
            // Already stopped.
        }
    });
    local.sources.clear();
    local.nextPlayTime = 0;
    clearTimeout(local.playingTimer);
}

function voiceAnalyserLevel(analyser, samples) {
    analyser.getFloatTimeDomainData(samples);
    let sum = 0;
    for (let i = 0; i < samples.length; i++) sum += samples[i] * samples[i];
    return Math.sqrt(sum / samples.length);
}

// The orb follows the louder of the microphone and the reply in this tab.
function startVoiceMeter(local) {
    const samples = new Float32Array(512);
    let smoothed = 0;
    const tick = function() {
        if (voiceLocal !== local) return;
        const input = local.muted ? 0 : voiceAnalyserLevel(local.inputAnalyser, samples);
        const output = voiceAnalyserLevel(local.outputAnalyser, samples);
        const level = Math.min(1, Math.max(input, output) * 4);
        smoothed = smoothed * 0.8 + level * 0.2;
        voiceOrbEl.style.setProperty('--voice-level', smoothed.toFixed(3));
        local.meterFrame = requestAnimationFrame(tick);
    };
    local.meterFrame = requestAnimationFrame(tick);
}

// teardownLocalVoice releases everything a browser session holds. It runs
// once per session; reason, when set, is shown to the user.
function teardownLocalVoice(local, reason) {
    if (local.ended) return;
    local.ended = true;
    clearTimeout(local.startTimer);
    cancelAnimationFrame(local.meterFrame);
    local.capturing = false;
    if (local.socket && local.socket.readyState <= WebSocket.OPEN) local.socket.close();
    if (local.capture) local.capture.port.onmessage = null;
    if (local.stream) local.stream.getTracks().forEach(track => track.stop());
    stopVoicePlayback(local);
    if (local.context) local.context.close().catch(() => {});
    if (voiceLocal === local) voiceLocal = null;
    voiceActivity = 'idle';
    // The session this tab carried is over. Do not wait for the next poll to
    // say so, or the page would briefly show it as running in another browser.
    if (voiceStatus.route === 'web') patchVoiceStatus({state: 'idle'});
    if (reason) setComposerHint(reason, true);
    renderRealtimeVoice();
    refreshRealtimeVoiceStatus();
}

async function endRealtimeVoice() {
    if (voiceStopping) return;
    if (voiceLocal) teardownLocalVoice(voiceLocal, '');
    // Show the end at once; the agent confirms it on the refresh below.
    patchVoiceStatus({state: 'idle'});
    voiceActivity = 'idle';
    voiceStopping = true;
    renderRealtimeVoice();
    try {
        await fetch('/api/realtime/voice/stop', {method: 'POST'});
    } catch (err) {
        setComposerHint('Could not end the voice session', true);
    } finally {
        voiceStopping = false;
    }
    refreshRealtimeVoiceStatus();
}

// applyLocalVoiceMute puts a browser session's mute state on its audio
// resources. The buttons work while the microphone prompt is still open,
// before the stream and the output exist, so this also runs as each appears.
function applyLocalVoiceMute(local) {
    // A disabled track captures silence, so nothing leaves the browser.
    if (local.stream) local.stream.getAudioTracks().forEach(track => { track.enabled = !local.muted; });
    if (local.outputGain) local.outputGain.gain.value = local.speakerMuted ? 0 : 1;
}

async function toggleRealtimeVoiceMute() {
    const muted = !voiceView().muted;
    if (voiceLocal) {
        voiceLocal.muted = muted;
        applyLocalVoiceMute(voiceLocal);
    }
    patchVoiceStatus({muted});
    renderRealtimeVoice();
    await postRealtimeVoiceMute({muted}, 'Could not change the microphone state');
}

// The agent silences the reply on either audio route; a browser session also
// mutes its own output so speech already queued in this tab stops at once.
async function toggleRealtimeVoiceSpeaker() {
    const speakerMuted = !voiceView().speakerMuted;
    if (voiceLocal) {
        voiceLocal.speakerMuted = speakerMuted;
        applyLocalVoiceMute(voiceLocal);
    }
    patchVoiceStatus({speaker_muted: speakerMuted});
    renderRealtimeVoice();
    await postRealtimeVoiceMute({speaker_muted: speakerMuted}, 'Could not change the speaker state');
}

async function postRealtimeVoiceMute(body, failure) {
    try {
        const res = await fetch('/api/realtime/voice/mute', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify(body)
        });
        // 409: the session is not running yet, or no longer. A browser
        // session applies its state again once capture starts.
        if (!res.ok && res.status !== 409) throw new Error(await res.text());
    } catch (err) {
        setComposerHint(failure, true);
    }
    refreshRealtimeVoiceStatus();
}

window.addEventListener('pagehide', function() {
    if (voiceLocal) teardownLocalVoice(voiceLocal, '');
});
