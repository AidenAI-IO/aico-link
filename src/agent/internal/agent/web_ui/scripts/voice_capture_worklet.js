// Audio worklet for browser realtime voice. It forwards the first microphone
// channel to the page, which resamples it to the agent's capture format.
class AicoVoiceCapture extends AudioWorkletProcessor {
    process(inputs) {
        const input = inputs[0];
        if (input && input[0] && input[0].length) {
            this.port.postMessage(input[0].slice(0));
        }
        return true;
    }
}

registerProcessor('aico-voice-capture', AicoVoiceCapture);
