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
const voiceCaptureFrameMs = 20;
const voicePlaybackLeadSeconds = 0.04;
const defaultInputPlaceholder = inputEl ? inputEl.placeholder : '';

let voiceStatus = {available: false, state: 'idle', muted: false, speaker_muted: false};
let voiceActivity = 'idle';
let voiceLocal = null;
let voiceStatusTimer = null;
let voiceStatusRefreshPending = false;

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
    const count = Math.max(1, channels || 1);
    const bytes = buffer.byteLength - (buffer.byteLength % (2 * count));
    const pcm = new Int16Array(buffer, 0, bytes / 2);
    const frames = pcm.length / count;
    const result = [];
    for (let c = 0; c < count; c++) {
        const channel = new Float32Array(frames);
        for (let i = 0; i < frames; i++) channel[i] = pcm[i * count + c] / 0x8000;
        result.push(channel);
    }
    return result;
}

function voiceModeActive() {
    return !!voiceLocal || voiceStatus.state === 'connecting' || voiceStatus.state === 'active';
}

function voiceStatusLabel() {
    if (voiceLocal && !voiceLocal.capturing) return 'Connecting…';
    if (!voiceLocal && voiceStatus.state === 'connecting') return 'Connecting…';
    const speakerMuted = !!(voiceStatus.speaker_muted || (voiceLocal && voiceLocal.speakerMuted));
    if (voiceStatus.muted && speakerMuted) return 'Microphone and speaker muted';
    if (voiceStatus.muted) return 'Microphone muted';
    if (speakerMuted && voiceActivity === 'speaking') return 'Speaker muted';
    const where = voiceLocal ? '' : voiceStatus.route === 'web' ? ' · audio in another browser' : ' · audio on device';
    switch (voiceActivity) {
        case 'listening': return 'Listening' + where;
        case 'thinking': return 'Thinking' + where;
        case 'speaking': return 'Speaking' + where;
        default: return 'Ready' + where;
    }
}

// The orb floats just above the composer, which grows with its input and
// attachments, so follow its height instead of a fixed offset.
const voiceComposerEl = document.querySelector('.composer');
if (voiceComposerEl && window.ResizeObserver) {
    new ResizeObserver(function() {
        voiceStageEl.parentElement.style.setProperty('--voice-composer-height', voiceComposerEl.offsetHeight + 'px');
    }).observe(voiceComposerEl);
}

function renderRealtimeVoice() {
    const active = voiceModeActive();
    const wasActive = document.body.classList.contains('voice-mode');
    voiceStageEl.classList.toggle('hidden', !active);
    document.body.classList.toggle('voice-mode', active);
    const muted = !!(voiceStatus.muted || (voiceLocal && voiceLocal.muted));
    const speakerMuted = !!(voiceStatus.speaker_muted || (voiceLocal && voiceLocal.speakerMuted));
    renderVoiceToggle(voiceMuteBtn, active, muted, 'microphone');
    renderVoiceToggle(voiceSpeakerBtn, active, speakerMuted, 'speaker');
    if (typeof renderComposerPrimary === 'function') renderComposerPrimary();
    const connecting = (voiceLocal && !voiceLocal.capturing) || (!voiceLocal && voiceStatus.state === 'connecting');
    voiceOrbEl.dataset.activity = connecting ? 'connecting' : muted && voiceActivity === 'listening' ? 'idle' : voiceActivity;
    voiceStatusEl.textContent = voiceStatusLabel();
    if (inputEl) inputEl.placeholder = active ? 'Type a message…' : defaultInputPlaceholder;
    if (!active) voiceOrbEl.style.removeProperty('--voice-level');
    // Entering voice mode adds room under the last message for the orb; keep
    // the reader at the bottom so the latest message stays above the fade.
    if (active && !wasActive) scrollToBottom();
}

function renderVoiceToggle(button, visible, off, name) {
    button.classList.toggle('hidden', !visible);
    button.classList.toggle('off', off);
    button.setAttribute('aria-pressed', off ? 'true' : 'false');
    button.title = (off ? 'Unmute ' : 'Mute ') + name;
    button.setAttribute('aria-label', button.title);
}

async function refreshRealtimeVoiceStatus() {
    if (voiceStatusRefreshPending) return;
    voiceStatusRefreshPending = true;
    try {
        const res = await fetch('/api/realtime/voice', {cache: 'no-store'});
        if (!res.ok) throw new Error(await res.text());
        const next = await res.json();
        const wasActive = voiceStatus.state !== 'idle';
        voiceStatus = Object.assign({available: false, state: 'idle', muted: false, speaker_muted: false}, next);
        if (voiceStatus.state === 'idle' || !wasActive) voiceActivity = 'idle';
    } catch (err) {
        // An older agent has no voice endpoint; keep the button hidden.
        voiceStatus = {available: false, state: 'idle', muted: false, speaker_muted: false};
    } finally {
        voiceStatusRefreshPending = false;
    }
    renderRealtimeVoice();
}

function startRealtimeVoiceStatusPolling() {
    refreshRealtimeVoiceStatus().then(startRealtimeVoiceFromLocation);
    if (voiceStatusTimer) clearInterval(voiceStatusTimer);
    voiceStatusTimer = setInterval(function() {
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
    if (voiceLocal || voiceModeActive()) return;
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
        socket: null, stream: null, context: null, capture: null, resampler: null,
        captureFormat: null, playbackFormat: {sample_rate: 24000, channels: 1, bit_width: 16},
        capturing: false, muted: false, speakerMuted: false, pending: [], pendingSamples: 0,
        sources: new Set(), nextPlayTime: 0, inputAnalyser: null, outputAnalyser: null, outputGain: null,
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
        openVoiceSocket(local);
        startVoiceMeter(local);
    } catch (err) {
        const reason = local.ended ? '' : voiceStartErrorMessage(err);
        teardownLocalVoice(local, reason);
    }
}

// realtimeVoiceSecureURL returns this page on the agent's HTTPS port, with a
// marker that starts voice once it loads, or '' when HTTPS is unavailable.
function realtimeVoiceSecureURL() {
    const port = Number(voiceStatus.https_port) || 0;
    if (!port || location.protocol === 'https:') return '';
    const url = new URL(location.href);
    url.protocol = 'https:';
    url.port = String(port);
    url.hash = 'voice';
    return url.toString();
}

// A page opened from realtimeVoiceSecureURL asks for one click to start.
// Starting on load would run without a user gesture, and browsers keep the
// audio of such a page suspended.
function startRealtimeVoiceFromLocation() {
    if (location.hash !== '#voice') return;
    history.replaceState(null, '', location.pathname + location.search);
    if (!voiceStatus.available || voiceModeActive()) return;
    setComposerHint('Press the voice button to start talking.', false);
    if (composerHintTimer) {
        clearTimeout(composerHintTimer);
        composerHintTimer = null;
    }
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
    socket.onclose = function() {
        teardownLocalVoice(local, local.endReason);
    };
    socket.onerror = function() {
        if (!local.endReason) local.endReason = 'Voice connection failed';
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
            local.captureFormat = message.format || {sample_rate: 16000, channels: 1, bit_width: 16};
            local.resampler = createVoiceResampler(local.context.sampleRate, local.captureFormat.sample_rate || 16000);
            local.pending = [];
            local.pendingSamples = 0;
            beginVoiceCapture(local);
            break;
        case 'capture_stop':
            local.capturing = false;
            break;
        case 'playback_start':
            if (message.format) local.playbackFormat = message.format;
            stopVoicePlayback(local);
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
// How long to wait for a suspended audio context to start. A browser that
// does not allow audio leaves resume() pending rather than rejecting it.
const voiceResumeTimeoutMs = 2000;

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
    if (local.startTimer) clearTimeout(local.startTimer);
    refreshRealtimeVoiceStatus();
    renderRealtimeVoice();
}

// Keep at most this much microphone audio waiting to be sent. On a slow link
// newer speech matters more than a backlog, which would reach the agent long
// after it was spoken.
const voiceSendBacklogBytes = 32000;

function handleVoiceCaptureFrame(local, samples) {
    if (!local.capturing || !local.resampler || !local.socket || local.socket.readyState !== WebSocket.OPEN) return;
    const resampled = local.resampler(samples);
    if (!resampled.length) return;
    local.pending.push(resampled);
    local.pendingSamples += resampled.length;
    const rate = local.captureFormat.sample_rate || 16000;
    if (local.pendingSamples < rate * voiceCaptureFrameMs / 1000) return;
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
        local.droppedFrames = (local.droppedFrames || 0) + 1;
        if (local.droppedFrames === 1) console.warn('[voice] connection is slow; dropping microphone audio');
        return;
    }
    local.droppedFrames = 0;
    local.socket.send(floatToPCM16(frame).buffer);
}

function playVoicePCM(local, buffer) {
    if (!local.context || !buffer || buffer.byteLength < 2) return;
    const format = local.playbackFormat || {};
    const channels = pcm16ToChannels(buffer, format.channels || 1);
    if (!channels[0].length) return;
    const audioBuffer = local.context.createBuffer(channels.length, channels[0].length, format.sample_rate || 24000);
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
}

function voiceAnalyserLevel(analyser, samples) {
    if (!analyser) return 0;
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

function teardownLocalVoice(local, reason) {
    if (local.ended && voiceLocal !== local) {
        // Already torn down; still release resources that arrived since.
        if (local.stream) local.stream.getTracks().forEach(track => track.stop());
        return;
    }
    local.ended = true;
    if (local.startTimer) clearTimeout(local.startTimer);
    if (local.meterFrame) cancelAnimationFrame(local.meterFrame);
    local.capturing = false;
    if (local.socket && local.socket.readyState <= WebSocket.OPEN) local.socket.close();
    if (local.capture) local.capture.port.onmessage = null;
    if (local.stream) local.stream.getTracks().forEach(track => track.stop());
    if (local.context) {
        stopVoicePlayback(local);
        local.context.close().catch(() => {});
    }
    if (voiceLocal === local) voiceLocal = null;
    voiceActivity = 'idle';
    if (reason) setComposerHint(reason, true);
    renderRealtimeVoice();
    refreshRealtimeVoiceStatus();
}

async function endRealtimeVoice() {
    const local = voiceLocal;
    if (local) {
        local.endReason = '';
        teardownLocalVoice(local, '');
    }
    try {
        await fetch('/api/realtime/voice/stop', {method: 'POST'});
    } catch (err) {
        console.error('Failed to stop voice:', err);
    }
    voiceActivity = 'idle';
    refreshRealtimeVoiceStatus();
}

async function toggleRealtimeVoiceMute() {
    const muted = !(voiceStatus.muted || (voiceLocal && voiceLocal.muted));
    if (voiceLocal) {
        voiceLocal.muted = muted;
        // A disabled track captures silence, so nothing leaves the browser.
        if (voiceLocal.stream) voiceLocal.stream.getAudioTracks().forEach(track => { track.enabled = !muted; });
    }
    voiceStatus = Object.assign({}, voiceStatus, {muted});
    renderRealtimeVoice();
    await postRealtimeVoiceMute({muted}, 'Could not change the microphone state');
}

// The agent silences the reply on either audio route; a browser session also
// mutes its own output so speech already queued in this tab stops at once.
async function toggleRealtimeVoiceSpeaker() {
    const speakerMuted = !(voiceStatus.speaker_muted || (voiceLocal && voiceLocal.speakerMuted));
    if (voiceLocal) {
        voiceLocal.speakerMuted = speakerMuted;
        if (voiceLocal.outputGain) voiceLocal.outputGain.gain.value = speakerMuted ? 0 : 1;
    }
    voiceStatus = Object.assign({}, voiceStatus, {speaker_muted: speakerMuted});
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
        if (!res.ok && res.status !== 409) throw new Error(await res.text());
    } catch (err) {
        setComposerHint(failure, true);
    }
    refreshRealtimeVoiceStatus();
}

window.addEventListener('pagehide', function() {
    if (voiceLocal) teardownLocalVoice(voiceLocal, '');
});
